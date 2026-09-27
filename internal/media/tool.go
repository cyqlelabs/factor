package media

import (
	"context"
	"strings"

	"github.com/cyqlelabs/factor/internal/tools"
)

// Tool is `media`: the one way the agent plays sound on the machine.
type Tool struct {
	player *Player
	guard  *tools.PathGuard
}

// NewTool wraps a player. guard decides which local files may be played.
func NewTool(player *Player, guard *tools.PathGuard) *Tool {
	return &Tool{player: player, guard: guard}
}

func (t *Tool) Name() string { return "media" }

func (t *Tool) Description() string {
	return "Play music and audio on this machine's speakers: internet radio streams, direct audio URLs, " +
		"pages the player can resolve through yt-dlp (YouTube, Bandcamp, SoundCloud), local files, or " +
		"ytsearch:<query> for the first YouTube result. This is the only way to play audio: never start " +
		"ffplay, vlc, mpv or paplay from exec, and never stop playback by killing processes — paplay is " +
		"your own voice. play replaces what is playing and reports success only once the source has " +
		"opened and playback is advancing, naming the title; a failure names the cause, so believe it " +
		"over a retry. queue appends after what is playing. status says what is playing, how far in, " +
		"and what is left in the queue — ask it before answering what is on. You are told when the queue " +
		"runs out, so do not poll for it. volume takes 0-100."
}

func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []any{"play", "queue", "pause", "resume", "next", "stop", "status", "volume"},
				"description": "What to do",
			},
			"source": map[string]any{
				"type":        "string",
				"description": "For play and queue: a URL, a local file path, or ytsearch:<query>",
			},
			"volume": map[string]any{
				"type":        "integer",
				"description": "For volume: the level, 0 to 100",
			},
		},
		"required": []any{"action"},
	}
}

func (t *Tool) Execute(ctx context.Context, args map[string]any) *tools.Result {
	action := tools.StringArg(args, "action")
	switch action {
	case "play", "queue":
		source, err := t.resolve(tools.StringArg(args, "source"))
		if err != nil {
			return tools.Errorf("%v", err)
		}
		tc := tools.ToolContextFrom(ctx)
		origin := Origin{Channel: tc.Channel, ChatID: tc.ChatID, Audience: tc.Audience}
		st, err := t.player.Play(ctx, source, action == "queue", origin)
		if err != nil {
			return tools.Errorf("%v", err)
		}
		if action == "queue" && st.Queued > 0 {
			return tools.Textf("Queued %s. %s", source, st)
		}
		return tools.Text(st.String())
	case "pause":
		return t.report(ctx, t.player.Pause(ctx))
	case "resume":
		return t.report(ctx, t.player.Resume(ctx))
	case "next":
		return t.report(ctx, t.player.Next(ctx))
	case "stop":
		return t.report(ctx, t.player.Stop(ctx))
	case "volume":
		return t.report(ctx, t.player.SetVolume(ctx, tools.IntArg(args, "volume", -1)))
	case "status":
		return tools.Text(t.player.Status(ctx).String())
	default:
		return tools.Errorf("unknown action %q; use play, queue, pause, resume, next, stop, status or volume", action)
	}
}

func (t *Tool) report(ctx context.Context, err error) *tools.Result {
	if err != nil {
		return tools.Errorf("%v", err)
	}
	return tools.Text(t.player.Status(ctx).String())
}

// resolve turns what the model asked for into something mpv opens: a search
// becomes a ytdl search, a URL passes through, and a local path has to be one
// the workspace rules allow reading.
func (t *Tool) resolve(source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", errSource
	}
	if q, ok := strings.CutPrefix(source, "ytsearch:"); ok {
		return "ytdl://ytsearch1:" + strings.TrimSpace(q), nil
	}
	if strings.Contains(source, "://") {
		return source, nil
	}
	if t.guard == nil {
		return source, nil
	}
	path, err := t.guard.CheckRead(source)
	if err != nil {
		return "", err
	}
	return path, nil
}

var errSource = errString("source is required: a URL, a file path, or ytsearch:<query>")

type errString string

func (e errString) Error() string { return string(e) }
