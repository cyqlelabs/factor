package desktop

import (
	"context"
	"fmt"
	"strings"
)

// windowsController drives Windows through PowerShell: Get-Process for the
// window list, a small user32 P/Invoke shim for focus/move/mouse, .NET for
// screenshots and SendKeys for typing. No external downloads, nothing to
// install — PowerShell ships with the OS.
type windowsController struct{ env Env }

func (c *windowsController) Backend() string { return "windows" }

func (c *windowsController) Helpers() []Helper {
	return []Helper{{Bin: "powershell", Purpose: "windows, input, screenshots, clipboard"}}
}

// psPrelude declares the user32 entry points the controller needs. It is
// prepended to every script that touches windows or the mouse.
const psPrelude = `Add-Type @"
using System;
using System.Runtime.InteropServices;
public class FactorWin {
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  [DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);
  [DllImport("user32.dll")] public static extern bool ShowWindow(IntPtr h, int c);
  [DllImport("user32.dll")] public static extern bool MoveWindow(IntPtr h, int x, int y, int w, int t, bool repaint);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool PostMessage(IntPtr h, uint m, IntPtr w, IntPtr l);
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern void mouse_event(uint f, int x, int y, uint d, int extra);
  [DllImport("user32.dll")] public static extern IntPtr GetForegroundWindow();
}
"@
`

// psQuote renders a Go string as a PowerShell single-quoted literal.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func (c *windowsController) ps(ctx context.Context, script string) (string, error) {
	shell, ok := c.env.first("powershell", "pwsh")
	if !ok {
		return "", missingHelper("controlling the desktop", "powershell")
	}
	return c.env.Run(ctx, "", shell, "-NoProfile", "-NonInteractive", "-Command", script)
}

// psListScript prints one tab-separated row per window. Fields are joined
// with [char]9 rather than an escaped tab so the script survives Go's raw
// strings, PowerShell's parser, and the reader all at once.
const psListScript = psPrelude + `$t = [char]9
Get-Process | Where-Object { $_.MainWindowTitle -ne '' } | ForEach-Object {
  $r = New-Object FactorWin+RECT
  [void][FactorWin]::GetWindowRect($_.MainWindowHandle, [ref]$r)
  ($_.MainWindowHandle, $_.Id, $_.ProcessName, $r.Left, $r.Top, ($r.Right - $r.Left), ($r.Bottom - $r.Top), $_.MainWindowTitle) -join $t
}`

func (c *windowsController) ListWindows(ctx context.Context) ([]Window, error) {
	out, err := c.ps(ctx, psListScript)
	if err != nil {
		return nil, err
	}
	var wins []Window
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 8 {
			continue
		}
		wins = append(wins, Window{
			ID: f[0], PID: atoi(f[1]), App: f[2],
			X: atoi(f[3]), Y: atoi(f[4]), W: atoi(f[5]), H: atoi(f[6]),
			Title: f[7], HasGeom: true,
		})
	}
	return wins, nil
}

func (c *windowsController) ActiveWindow(ctx context.Context) (Window, error) {
	out, err := c.ps(ctx, psPrelude+`[FactorWin]::GetForegroundWindow().ToString()`)
	if err != nil {
		return Window{}, err
	}
	id := strings.TrimSpace(out)
	if id == "" || id == "0" {
		return Window{}, fmt.Errorf("no foreground window")
	}
	return Window{ID: id}, nil
}

func (c *windowsController) handle(w Window) string {
	return fmt.Sprintf("[IntPtr]::new([int64]%s)", psQuote(w.ID))
}

func (c *windowsController) Focus(ctx context.Context, w Window) error {
	_, err := c.ps(ctx, psPrelude+fmt.Sprintf(`[void][FactorWin]::ShowWindow(%s, 9); [void][FactorWin]::SetForegroundWindow(%s)`,
		c.handle(w), c.handle(w)))
	return err
}

func (c *windowsController) CloseWindow(ctx context.Context, w Window) error {
	const wmClose = "0x0010"
	_, err := c.ps(ctx, psPrelude+fmt.Sprintf(`[void][FactorWin]::PostMessage(%s, %s, [IntPtr]::Zero, [IntPtr]::Zero)`,
		c.handle(w), wmClose))
	return err
}

// showWindowCmds are the SW_* constants for ShowWindow.
var showWindowCmds = map[string]int{
	"minimize": 6, "maximize": 3, "restore": 9, "fullscreen": 3, "unfullscreen": 9,
}

func (c *windowsController) SetState(ctx context.Context, w Window, state string) error {
	cmd, ok := showWindowCmds[state]
	if !ok {
		return fmt.Errorf("unknown window state %q", state)
	}
	_, err := c.ps(ctx, psPrelude+fmt.Sprintf(`[void][FactorWin]::ShowWindow(%s, %d)`, c.handle(w), cmd))
	return err
}

func (c *windowsController) MoveResize(ctx context.Context, w Window, g Geometry) error {
	x, y, width, height := g.X, g.Y, g.W, g.H
	if !g.HasPos || !g.HasSize {
		// MoveWindow needs all four; fill the missing half from the current rect.
		current, err := c.ListWindows(ctx)
		if err != nil {
			return err
		}
		for _, cw := range current {
			if cw.ID == w.ID {
				if !g.HasPos {
					x, y = cw.X, cw.Y
				}
				if !g.HasSize {
					width, height = cw.W, cw.H
				}
				break
			}
		}
	}
	_, err := c.ps(ctx, psPrelude+fmt.Sprintf(`[void][FactorWin]::MoveWindow(%s, %d, %d, %d, %d, $true)`,
		c.handle(w), x, y, width, height))
	return err
}

func (c *windowsController) Screenshot(ctx context.Context, path string, shot Shot) error {
	region := shot.Region
	if shot.Mode == "window" {
		if err := c.Focus(ctx, shot.Window); err != nil {
			return err
		}
		if !shot.Window.HasGeom {
			return unsupported("window screenshots without geometry", "windows", "call window_list first")
		}
		region = Geometry{X: shot.Window.X, Y: shot.Window.Y, W: shot.Window.W, H: shot.Window.H, HasPos: true, HasSize: true}
		shot.Mode = "region"
	}
	bounds := `$b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds`
	if shot.Mode == "region" {
		bounds = fmt.Sprintf(`$b = New-Object Drawing.Rectangle(%d, %d, %d, %d)`, region.X, region.Y, region.W, region.H)
	}
	script := `Add-Type -AssemblyName System.Drawing, System.Windows.Forms
` + bounds + `
$bmp = New-Object Drawing.Bitmap($b.Width, $b.Height)
$g = [Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($b.Left, $b.Top, 0, 0, $bmp.Size)
$bmp.Save(` + psQuote(path) + `, [Drawing.Imaging.ImageFormat]::Png)
$g.Dispose(); $bmp.Dispose()`
	_, err := c.ps(ctx, script)
	return err
}

func (c *windowsController) MoveMouse(ctx context.Context, x, y int) error {
	_, err := c.ps(ctx, psPrelude+fmt.Sprintf(`[void][FactorWin]::SetCursorPos(%d, %d)`, x, y))
	return err
}

// mouseEventFlags are the MOUSEEVENTF_* down/up pairs per button.
var mouseEventFlags = map[string][2]string{
	"left":   {"0x0002", "0x0004"},
	"right":  {"0x0008", "0x0010"},
	"middle": {"0x0020", "0x0040"},
}

func (c *windowsController) Click(ctx context.Context, button string, count int, at *Point) error {
	flags, ok := mouseEventFlags[button]
	if !ok {
		return fmt.Errorf("unknown mouse button %q", button)
	}
	if count < 1 {
		count = 1
	}
	var b strings.Builder
	b.WriteString(psPrelude)
	if at != nil {
		fmt.Fprintf(&b, "[void][FactorWin]::SetCursorPos(%d, %d)\n", at.X, at.Y)
	}
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "[FactorWin]::mouse_event(%s, 0, 0, 0, 0)\n[FactorWin]::mouse_event(%s, 0, 0, 0, 0)\n", flags[0], flags[1])
		if i+1 < count {
			b.WriteString("Start-Sleep -Milliseconds 80\n")
		}
	}
	_, err := c.ps(ctx, b.String())
	return err
}

// sendKeysEscape protects the characters SendKeys treats as syntax.
func sendKeysEscape(s string) string {
	r := strings.NewReplacer("{", "{{}", "}", "{}}", "+", "{+}", "^", "{^}", "%", "{%}", "~", "{~}", "(", "{(}", ")", "{)}", "[", "{[}", "]", "{]}")
	return r.Replace(s)
}

func (c *windowsController) TypeText(ctx context.Context, text string, _ int) error {
	script := `Add-Type -AssemblyName System.Windows.Forms
[System.Windows.Forms.SendKeys]::SendWait(` + psQuote(sendKeysEscape(text)) + `)`
	_, err := c.ps(ctx, script)
	return err
}

// sendKeysModifiers and sendKeysNames translate portable key names to the
// SendKeys dialect.
var sendKeysModifiers = map[string]string{
	"ctrl": "^", "control": "^", "alt": "%", "shift": "+", "win": "^{ESC}", "super": "^{ESC}",
}

var sendKeysNames = map[string]string{
	"return": "{ENTER}", "enter": "{ENTER}", "tab": "{TAB}", "escape": "{ESC}", "esc": "{ESC}",
	"space": " ", "backspace": "{BACKSPACE}", "delete": "{DELETE}", "up": "{UP}", "down": "{DOWN}",
	"left": "{LEFT}", "right": "{RIGHT}", "home": "{HOME}", "end": "{END}",
	"pageup": "{PGUP}", "pagedown": "{PGDN}", "insert": "{INSERT}",
	"f1": "{F1}", "f2": "{F2}", "f3": "{F3}", "f4": "{F4}", "f5": "{F5}", "f6": "{F6}",
	"f7": "{F7}", "f8": "{F8}", "f9": "{F9}", "f10": "{F10}", "f11": "{F11}", "f12": "{F12}",
}

func (c *windowsController) PressKey(ctx context.Context, keys string, repeat int) error {
	parts := strings.Split(strings.ToLower(keys), "+")
	var prefix string
	for _, m := range parts[:len(parts)-1] {
		sk, ok := sendKeysModifiers[m]
		if !ok {
			return fmt.Errorf("unknown modifier %q", m)
		}
		prefix += sk
	}
	key := parts[len(parts)-1]
	seq, ok := sendKeysNames[key]
	if !ok {
		if len([]rune(key)) != 1 {
			return fmt.Errorf("unknown key %q", key)
		}
		seq = sendKeysEscape(key)
	}
	if repeat < 1 {
		repeat = 1
	}
	script := `Add-Type -AssemblyName System.Windows.Forms
` + fmt.Sprintf("for ($i = 0; $i -lt %d; $i++) { [System.Windows.Forms.SendKeys]::SendWait(%s) }",
		repeat, psQuote(prefix+seq))
	_, err := c.ps(ctx, script)
	return err
}

func (c *windowsController) ClipboardGet(ctx context.Context) (string, error) {
	return c.ps(ctx, `Get-Clipboard -Raw`)
}

func (c *windowsController) ClipboardSet(ctx context.Context, text string) error {
	_, err := c.ps(ctx, `Set-Clipboard -Value `+psQuote(text))
	return err
}

func (c *windowsController) Notify(ctx context.Context, title, body, _ string) error {
	script := `Add-Type -AssemblyName System.Windows.Forms
$n = New-Object System.Windows.Forms.NotifyIcon
$n.Icon = [System.Drawing.SystemIcons]::Information
$n.Visible = $true
$n.ShowBalloonTip(5000, ` + psQuote(title) + `, ` + psQuote(body) + `, [System.Windows.Forms.ToolTipIcon]::Info)
Start-Sleep -Seconds 5
$n.Dispose()`
	_, err := c.ps(ctx, `Add-Type -AssemblyName System.Drawing
`+script)
	return err
}

func (c *windowsController) Open(ctx context.Context, target string) error {
	_, err := c.ps(ctx, `Start-Process `+psQuote(target))
	return err
}

func (c *windowsController) ScreenSize(ctx context.Context) (int, int, error) {
	out, err := c.ps(ctx, `Add-Type -AssemblyName System.Windows.Forms
$b = [System.Windows.Forms.Screen]::PrimaryScreen.Bounds
"$($b.Width) $($b.Height)"`)
	if err != nil {
		return 0, 0, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("unexpected screen bounds %q", strings.TrimSpace(out))
	}
	return atoi(f[0]), atoi(f[1]), nil
}
