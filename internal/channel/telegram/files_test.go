package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cyqlelabs/factor/internal/bus"
	"github.com/cyqlelabs/factor/internal/tools"
)

// fileAPI fakes the Bot API surface the file paths touch: getFile, the
// /file/bot download tree, and the two multipart upload methods.
type fileAPI struct {
	mu          sync.Mutex
	getFileIDs  []string
	filePath    string // what getFile hands back
	fileBody    []byte // what the download tree serves
	uploads     []upload
	rejectPhoto bool
	failGetFile bool
}

type upload struct {
	method    string
	fields    map[string]string
	fileField string
	filename  string
	body      []byte
}

func (f *fileAPI) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getFile"):
			var body struct {
				FileID string `json:"file_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.getFileIDs = append(f.getFileIDs, body.FileID)
			fail, filePath := f.failGetFile, f.filePath
			f.mu.Unlock()
			if fail {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"ok":false,"description":"Bad Request: file is too big"}`)
				return
			}
			fmt.Fprintf(w, `{"ok":true,"result":{"file_path":%q}}`, filePath)
		case strings.Contains(r.URL.Path, "/file/bot"):
			f.mu.Lock()
			body := f.fileBody
			f.mu.Unlock()
			_, _ = w.Write(body)
		case strings.HasSuffix(r.URL.Path, "/sendPhoto"), strings.HasSuffix(r.URL.Path, "/sendDocument"):
			up := recordUpload(t, r)
			f.mu.Lock()
			f.uploads = append(f.uploads, up)
			reject := f.rejectPhoto && up.method == "sendPhoto"
			f.mu.Unlock()
			if reject {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"ok":false,"description":"Bad Request: PHOTO_INVALID_DIMENSIONS"}`)
				return
			}
			fmt.Fprint(w, `{"ok":true}`)
		default:
			fmt.Fprint(w, `{"ok":true,"result":[]}`)
		}
	}
}

func recordUpload(t *testing.T, r *http.Request) upload {
	t.Helper()
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		t.Errorf("upload is not multipart: %v", err)
		return upload{}
	}
	up := upload{method: path.Base(r.URL.Path), fields: map[string]string{}}
	for k, vs := range r.MultipartForm.Value {
		up.fields[k] = vs[0]
	}
	for field, fhs := range r.MultipartForm.File {
		up.fileField = field
		up.filename = fhs[0].Filename
		file, err := fhs[0].Open()
		if err != nil {
			t.Errorf("open uploaded file: %v", err)
			continue
		}
		up.body, _ = io.ReadAll(file)
		_ = file.Close()
	}
	return up
}

// newFileTelegram builds a guarded connector over the file fake, with a
// throwaway workspace.
func newFileTelegram(t *testing.T, api *fileAPI) (*Telegram, *bus.MessageBus, string) {
	t.Helper()
	tg, b := newTelegram(t, api.handler(t))
	ws := t.TempDir()
	tg.BindPathGuard(tools.NewPathGuard(ws, true, false, nil))
	return tg, b, ws
}

func receive(t *testing.T, b *bus.MessageBus) bus.InboundMessage {
	t.Helper()
	select {
	case msg := <-b.Inbound():
		return msg
	default:
		t.Fatal("no inbound message was published")
		return bus.InboundMessage{}
	}
}

func TestHandleDownloadsADocumentIntoTheWorkspace(t *testing.T) {
	api := &fileAPI{filePath: "documents/file_7.pdf", fileBody: []byte("%PDF fake")}
	tg, b, ws := newFileTelegram(t, api)

	tg.handle(context.Background(), mustUpdate(t, `{"update_id":1,"message":{
		"from":{"id":42,"username":"nico"},"chat":{"id":77},
		"caption":"read this","document":{"file_id":"DOC1","file_name":"report.pdf"}}}`))

	msg := receive(t, b)
	saved := filepath.Join(ws, "downloads", "report.pdf")
	data, err := os.ReadFile(saved)
	if err != nil || string(data) != "%PDF fake" {
		t.Fatalf("saved file = %q, %v; want the served bytes at %s", data, err, saved)
	}
	if msg.ChatID != "77" || msg.SenderID != "42" {
		t.Errorf("msg = %+v, want chat 77 from 42", msg)
	}
	if !strings.HasPrefix(msg.Content, "read this\n\n") || !strings.Contains(msg.Content, saved) {
		t.Errorf("content = %q, want the caption then the saved path", msg.Content)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.getFileIDs) != 1 || api.getFileIDs[0] != "DOC1" {
		t.Errorf("getFile asked for %v, want [DOC1]", api.getFileIDs)
	}
}

func TestHandleDownloadsTheLargestPhotoNamedByItsServerPath(t *testing.T) {
	api := &fileAPI{filePath: "photos/file_1.jpg", fileBody: []byte("jpegbytes")}
	tg, b, ws := newFileTelegram(t, api)

	tg.handle(context.Background(), mustUpdate(t, `{"update_id":2,"message":{
		"from":{"id":42},"chat":{"id":77},
		"photo":[{"file_id":"SMALL"},{"file_id":"BIG"}]}}`))

	msg := receive(t, b)
	saved := filepath.Join(ws, "downloads", "file_1.jpg")
	if data, err := os.ReadFile(saved); err != nil || string(data) != "jpegbytes" {
		t.Fatalf("saved photo = %q, %v; want the served bytes at %s", data, err, saved)
	}
	if !strings.Contains(msg.Content, "photo") || !strings.Contains(msg.Content, saved) {
		t.Errorf("content = %q, want a photo note with the saved path", msg.Content)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.getFileIDs) != 1 || api.getFileIDs[0] != "BIG" {
		t.Errorf("getFile asked for %v, want the largest size [BIG]", api.getFileIDs)
	}
}

func TestHandleDeliversTheMessageWhenTheDownloadFails(t *testing.T) {
	api := &fileAPI{failGetFile: true}
	tg, b, _ := newFileTelegram(t, api)

	tg.handle(context.Background(), mustUpdate(t, `{"update_id":3,"message":{
		"from":{"id":42},"chat":{"id":77},
		"caption":"huge","document":{"file_id":"DOC1","file_name":"big.iso"}}}`))

	msg := receive(t, b)
	if !strings.HasPrefix(msg.Content, "huge\n\n") || !strings.Contains(msg.Content, "failed") {
		t.Errorf("content = %q, want the caption and a failure note", msg.Content)
	}
	if strings.Contains(msg.Content, secretToken) {
		t.Errorf("content leaks the bot token: %q", msg.Content)
	}
}

func TestHandleSanitizesHostileFileNames(t *testing.T) {
	api := &fileAPI{filePath: "documents/file_9", fileBody: []byte("boo")}
	tg, b, ws := newFileTelegram(t, api)

	tg.handle(context.Background(), mustUpdate(t, `{"update_id":4,"message":{
		"from":{"id":42},"chat":{"id":77},
		"document":{"file_id":"DOC1","file_name":"../../evil.sh"}}}`))

	msg := receive(t, b)
	saved := filepath.Join(ws, "downloads", "evil.sh")
	if _, err := os.Stat(saved); err != nil {
		t.Fatalf("want the name reduced to its base and saved at %s: %v", saved, err)
	}
	if !strings.Contains(msg.Content, saved) {
		t.Errorf("content = %q, want the sanitized path", msg.Content)
	}
}

func TestHandleRefusesAFileOverTheDownloadLimit(t *testing.T) {
	api := &fileAPI{filePath: "documents/file_5.bin", fileBody: make([]byte, maxDownload+1)}
	tg, b, ws := newFileTelegram(t, api)

	tg.handle(context.Background(), mustUpdate(t, `{"update_id":5,"message":{
		"from":{"id":42},"chat":{"id":77},
		"document":{"file_id":"DOC1","file_name":"big.bin"}}}`))

	msg := receive(t, b)
	if !strings.Contains(msg.Content, "download limit") {
		t.Errorf("content = %q, want the size-limit failure note", msg.Content)
	}
	if _, err := os.Stat(filepath.Join(ws, "downloads", "big.bin")); !os.IsNotExist(err) {
		t.Error("a truncated download was left on disk")
	}
}

func TestDownloadDoesNotOverwriteAnExistingFile(t *testing.T) {
	api := &fileAPI{filePath: "documents/file_1.txt", fileBody: []byte("second")}
	tg, b, ws := newFileTelegram(t, api)
	dir := filepath.Join(ws, "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}

	tg.handle(context.Background(), mustUpdate(t, `{"update_id":6,"message":{
		"from":{"id":42},"chat":{"id":77},
		"document":{"file_id":"DOC1","file_name":"notes.txt"}}}`))

	receive(t, b)
	if data, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(data) != "first" {
		t.Errorf("existing file overwritten with %q", data)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "notes-2.txt")); err != nil || string(data) != "second" {
		t.Errorf("notes-2.txt = %q, %v; want the new file beside the old", data, err)
	}
}

func TestAttachmentExtraction(t *testing.T) {
	tests := map[string]struct {
		raw    string
		fileID string
		name   string
		kind   string
	}{
		"voice": {`{"voice":{"file_id":"V1"}}`, "V1", "", "voice message"},
		"audio": {`{"audio":{"file_id":"A1","file_name":"song.mp3"}}`, "A1", "song.mp3", "audio file"},
		"video": {`{"video":{"file_id":"VID1","file_name":"clip.mp4"}}`, "VID1", "clip.mp4", "video"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var m message
			if err := json.Unmarshal([]byte(tt.raw), &m); err != nil {
				t.Fatal(err)
			}
			att := m.attachment()
			if att == nil || att.fileID != tt.fileID || att.name != tt.name || att.kind != tt.kind {
				t.Errorf("attachment() = %+v, want {%s %s %s}", att, tt.fileID, tt.name, tt.kind)
			}
		})
	}
	var m message
	if err := json.Unmarshal([]byte(`{"text":"just text"}`), &m); err != nil {
		t.Fatal(err)
	}
	if att := m.attachment(); att != nil {
		t.Errorf("attachment() = %+v for a text message, want nil", att)
	}
}

func TestDownloadsDirFallsBackToTheDefaultHomeWithoutAGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("FACTOR_HOME", home)
	tg, _ := newTelegram(t, func(http.ResponseWriter, *http.Request) {})
	if got, want := tg.downloadsDir(), filepath.Join(home, "workspace", "downloads"); got != want {
		t.Errorf("downloadsDir() = %q, want %q", got, want)
	}
}

// toolCtx is a turn context as the agent loop builds it for a Telegram chat.
func toolCtx(chatID string) context.Context {
	return tools.WithToolContext(context.Background(),
		tools.ToolContext{Channel: "telegram", ChatID: chatID, SessionKey: "telegram:" + chatID})
}

func TestSendFileToolSendsADocument(t *testing.T) {
	api := &fileAPI{}
	tg, _, ws := newFileTelegram(t, api)
	if err := os.WriteFile(filepath.Join(ws, "report.pdf"), []byte("%PDF out"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := tg.Toolset()[0].Execute(toolCtx("77"), map[string]any{"path": "report.pdf", "caption": "the report"})
	if res.IsError {
		t.Fatalf("Execute = %q, want success", res.ForLLM)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.uploads) != 1 {
		t.Fatalf("uploads = %+v, want one sendDocument", api.uploads)
	}
	up := api.uploads[0]
	if up.method != "sendDocument" || up.fileField != "document" || up.filename != "report.pdf" ||
		string(up.body) != "%PDF out" || up.fields["chat_id"] != "77" || up.fields["caption"] != "the report" {
		t.Errorf("upload = %+v, want the document with its caption in chat 77", up)
	}
}

func TestSendFileToolSendsImagesAsPhotos(t *testing.T) {
	api := &fileAPI{}
	tg, _, ws := newFileTelegram(t, api)
	if err := os.WriteFile(filepath.Join(ws, "pic.PNG"), []byte("pngbytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := tg.Toolset()[0].Execute(toolCtx("77"), map[string]any{"path": "pic.PNG"})
	if res.IsError {
		t.Fatalf("Execute = %q, want success", res.ForLLM)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.uploads) != 1 || api.uploads[0].method != "sendPhoto" || api.uploads[0].fileField != "photo" {
		t.Errorf("uploads = %+v, want one sendPhoto", api.uploads)
	}
}

func TestSendFileToolFallsBackToADocumentWhenThePhotoIsRejected(t *testing.T) {
	api := &fileAPI{rejectPhoto: true}
	tg, _, ws := newFileTelegram(t, api)
	if err := os.WriteFile(filepath.Join(ws, "odd.jpg"), []byte("jpg"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := tg.Toolset()[0].Execute(toolCtx("77"), map[string]any{"path": "odd.jpg"})
	if res.IsError {
		t.Fatalf("Execute = %q, want the document fallback to deliver", res.ForLLM)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.uploads) != 2 || api.uploads[0].method != "sendPhoto" || api.uploads[1].method != "sendDocument" {
		t.Errorf("uploads = %+v, want sendPhoto then sendDocument", api.uploads)
	}
}

func TestSendFileToolRefusesPathsOutsideTheWorkspace(t *testing.T) {
	tg, _, _ := newFileTelegram(t, &fileAPI{})
	res := tg.Toolset()[0].Execute(toolCtx("77"), map[string]any{"path": "/etc/hostname"})
	if !res.IsError {
		t.Errorf("Execute = %q, want the guard to refuse a path outside the workspace", res.ForLLM)
	}
}

func TestSendFileToolRejectsDirectories(t *testing.T) {
	tg, _, _ := newFileTelegram(t, &fileAPI{})
	res := tg.Toolset()[0].Execute(toolCtx("77"), map[string]any{"path": "."})
	if !res.IsError || !strings.Contains(res.ForLLM, "directory") {
		t.Errorf("Execute = %q, want a directory refusal", res.ForLLM)
	}
}

func TestSendFileToolTargetsChats(t *testing.T) {
	api := &fileAPI{}
	tg, _ := newTelegram(t, api.handler(t), "77")
	ws := t.TempDir()
	tg.BindPathGuard(tools.NewPathGuard(ws, true, false, nil))
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := tg.Toolset()[0]

	if res := tool.Execute(context.Background(), map[string]any{"path": "f.txt"}); !res.IsError {
		t.Error("a turn with no Telegram chat and no chat_id must be refused")
	}
	if res := tool.Execute(context.Background(), map[string]any{"path": "f.txt", "chat_id": "999"}); !res.IsError {
		t.Error("a chat outside allow_from must be refused")
	}
	if res := tool.Execute(context.Background(), map[string]any{"path": "f.txt", "chat_id": "77"}); res.IsError {
		t.Errorf("an allowed explicit chat was refused: %q", res.ForLLM)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.uploads) != 1 || api.uploads[0].fields["chat_id"] != "77" {
		t.Errorf("uploads = %+v, want one send to chat 77", api.uploads)
	}
}

func TestSendFileToolNeedsABoundGuard(t *testing.T) {
	tg, _ := newTelegram(t, func(http.ResponseWriter, *http.Request) {})
	res := tg.Toolset()[0].Execute(toolCtx("77"), map[string]any{"path": "f.txt"})
	if !res.IsError || !strings.Contains(res.ForLLM, "gateway") {
		t.Errorf("Execute = %q, want a refusal without a path guard", res.ForLLM)
	}
}

func TestToolsetContributesTheSendFileTool(t *testing.T) {
	tg, _ := newTelegram(t, func(http.ResponseWriter, *http.Request) {})
	ts := tg.Toolset()
	if len(ts) != 1 || ts[0].Name() != "telegram_send_file" {
		t.Fatalf("Toolset() = %v, want the telegram_send_file tool", ts)
	}
	if ts[0].Description() == "" || ts[0].Parameters()["type"] != "object" {
		t.Error("the tool must describe itself and declare an object schema")
	}
}
