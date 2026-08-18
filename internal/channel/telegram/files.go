// files.go moves files across the chat, both ways: attachments the user
// sends are downloaded into the workspace and handed to the model as a local
// path, and the telegram_send_file tool lets the agent hand the user an
// actual file — a report it wrote, a screenshot, a download — instead of
// describing one.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cyqlelabs/factor/internal/config"
	"github.com/cyqlelabs/factor/internal/tools"
)

const (
	maxDownload = 20 << 20 // the Bot API refuses to serve larger files
	maxUpload   = 50 << 20 // the Bot API caps bot uploads here
)

// BindPathGuard gives the connector the path rules every file tool obeys.
// The gateway calls it before Start.
func (t *Telegram) BindPathGuard(g *tools.PathGuard) { t.guard = g }

// Toolset contributes the file-sending tool wherever Telegram is configured.
func (t *Telegram) Toolset() []tools.Tool { return []tools.Tool{&sendFileTool{t: t}} }

// attachment is the one file a Telegram message can carry, whatever Telegram
// calls it.
type attachment struct {
	fileID string
	name   string // the sender's name for it; most kinds have none
	kind   string // what to call it when telling the model
}

func (m *message) attachment() *attachment {
	switch {
	case m.Document != nil:
		return &attachment{m.Document.FileID, m.Document.FileName, "file"}
	case len(m.Photo) > 0:
		return &attachment{m.Photo[len(m.Photo)-1].FileID, "", "photo"} // sizes ascend; last is full
	case m.Voice != nil:
		return &attachment{m.Voice.FileID, "", "voice message"}
	case m.Audio != nil:
		return &attachment{m.Audio.FileID, m.Audio.FileName, "audio file"}
	case m.Video != nil:
		return &attachment{m.Video.FileID, m.Video.FileName, "video"}
	}
	return nil
}

// downloadsDir is where received files land: inside the workspace when the
// guard is bound (always, under the gateway), else under the default home.
func (t *Telegram) downloadsDir() string {
	if t.guard != nil {
		return filepath.Join(t.guard.Workspace(), "downloads")
	}
	return filepath.Join(config.Home(), "workspace", "downloads")
}

// download fetches one attachment and returns the local path it was saved to.
func (t *Telegram) download(ctx context.Context, att *attachment) (string, error) {
	var info struct {
		FilePath string `json:"file_path"`
	}
	if err := t.callResult(ctx, "getFile", map[string]any{"file_id": att.fileID}, &info); err != nil {
		return "", err
	}
	name := baseName(att.name)
	if name == "" {
		name = baseName(info.FilePath)
	}
	if name == "" {
		name = "file"
	}
	dir := t.downloadsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.fileBase+"/"+info.FilePath, nil)
	if err != nil {
		return "", t.redact(err) // the URL in this error embeds the token
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return "", t.redact(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("telegram file download failed: HTTP %d", resp.StatusCode)
	}
	dest := uniquePath(filepath.Join(dir, name))
	f, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxDownload {
		err = fmt.Errorf("the file exceeds the %d MB download limit", maxDownload>>20)
	}
	if err != nil {
		_ = os.Remove(dest)
		return "", err
	}
	return dest, nil
}

// baseName reduces an untrusted name from the API to a bare file name.
func baseName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == string(filepath.Separator) || name == ".." {
		return ""
	}
	return name
}

// uniquePath keeps existing files: a taken name becomes name-2.ext, name-3.ext, …
func uniquePath(target string) string {
	ext := filepath.Ext(target)
	stem := strings.TrimSuffix(target, ext)
	for i := 2; ; i++ {
		if _, err := os.Lstat(target); err != nil {
			return target
		}
		target = fmt.Sprintf("%s-%d%s", stem, i, ext)
	}
}

// sendFile picks the method by what the file is: images get a photo bubble,
// everything else arrives as a document. A photo Telegram refuses (too
// large, odd dimensions) goes again as a document rather than failing the
// send.
func (t *Telegram) sendFile(ctx context.Context, chatID, path, caption string) error {
	fields := map[string]string{"chat_id": chatID}
	if caption != "" {
		fields["caption"] = caption
	}
	if isImage(path) {
		err := t.upload(ctx, "sendPhoto", "photo", path, fields)
		var rejected *apiError
		if err == nil || !errors.As(err, &rejected) || rejected.status != http.StatusBadRequest {
			return err
		}
	}
	return t.upload(ctx, "sendDocument", "document", path, fields)
}

// isImage lists the formats sendPhoto accepts; a GIF stays a document so it
// keeps animating.
func isImage(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	}
	return false
}

// upload posts one file to a Bot API method as multipart/form-data — the one
// encoding that carries local files — streaming it rather than buffering.
func (t *Telegram) upload(ctx context.Context, method, fileField, path string, fields map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.apiBase+"/"+method, pr)
	if err != nil {
		return t.redact(err) // the URL in this error embeds the token
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	go func() {
		err := writeForm(mw, fields, fileField, filepath.Base(path), f)
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	resp, err := t.client.Do(req)
	if err != nil {
		return t.redact(err)
	}
	defer resp.Body.Close()
	return decodeAPI(method, resp, nil)
}

func writeForm(mw *multipart.Writer, fields map[string]string, fileField, filename string, r io.Reader) error {
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return err
		}
	}
	part, err := mw.CreateFormFile(fileField, filename)
	if err != nil {
		return err
	}
	_, err = io.Copy(part, r)
	return err
}

type sendFileTool struct{ t *Telegram }

func (s *sendFileTool) Name() string { return "telegram_send_file" }

func (s *sendFileTool) Description() string {
	return "Send a local file to a Telegram chat: a document, image, audio — any file the user should have in hand rather than hear about. Images arrive as photos, everything else as a document. Defaults to the chat of the current conversation."
}

func (s *sendFileTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "Path of the file to send"},
			"caption": map[string]any{"type": "string", "description": "Short plain-text caption shown with the file"},
			"chat_id": map[string]any{"type": "string", "description": "Numeric chat to send to; defaults to the current conversation's chat"},
		},
		"required": []any{"path"},
	}
}

func (s *sendFileTool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	chat, err := s.t.targetChat(ctx, strings.TrimSpace(tools.StringArg(args, "chat_id")))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	path, err := s.t.checkRead(tools.StringArg(args, "path"))
	if err != nil {
		return tools.Errorf("%v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return tools.Errorf("%v", err)
	}
	if fi.IsDir() {
		return tools.Errorf("%s is a directory; send a file", path)
	}
	if fi.Size() > maxUpload {
		return tools.Errorf("%s is %d MB; Telegram caps bot uploads at %d MB", path, fi.Size()>>20, maxUpload>>20)
	}
	if err := s.t.sendFile(ctx, chat, path, strings.TrimSpace(tools.StringArg(args, "caption"))); err != nil {
		return tools.Errorf("could not send the file: %v", s.t.redact(err))
	}
	return tools.Textf("Sent %s to Telegram chat %s.", filepath.Base(path), chat)
}

// targetChat resolves where a tool call should send: the chat named
// explicitly, or the chat whose turn is running. A chat outside the
// allowlist is an error, never a silent redirect.
func (t *Telegram) targetChat(ctx context.Context, explicit string) (string, error) {
	if explicit != "" {
		if len(t.allow) > 0 && !t.allow[explicit] {
			return "", fmt.Errorf("chat %s is not in channels.telegram.allow_from", explicit)
		}
		return explicit, nil
	}
	tc := tools.ToolContextFrom(ctx)
	if tc.Channel == "telegram" && tc.ChatID != "" {
		return tc.ChatID, nil
	}
	return "", fmt.Errorf("this conversation is not a Telegram chat; pass chat_id")
}

// checkRead applies the file tools' path rules; without a bound guard the
// connector refuses rather than reading unchecked.
func (t *Telegram) checkRead(path string) (string, error) {
	if t.guard == nil {
		return "", fmt.Errorf("file sending is only available under the gateway")
	}
	return t.guard.CheckRead(path)
}
