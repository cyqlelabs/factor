package voice

import (
	"html"
	"net/url"
	"regexp"
	"strings"

	"github.com/cyqlelabs/factor/internal/channel"
)

// The agent is told to answer voice turns in plain prose, but a prompt is a
// request, not a guarantee — and a synthesiser reads what it is given, so a
// model that slips into markdown has the speakers saying "asterisk asterisk".
// spokenText is the seatbelt: it rewrites a reply into something a voice can
// say, applied to every synthesis on every tier.

var (
	fencedBlock   = regexp.MustCompile("(?s)```[^\n]*\n?.*?```")
	danglingFence = regexp.MustCompile("(?s)```[^\n]*\n?.*$")
	inlineCode    = regexp.MustCompile("`([^`\n]*)`")
	mdImage       = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	mdLink        = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	mdBoldStars   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdBoldUnder   = regexp.MustCompile(`__([^_]+)__`)
	mdStrike      = regexp.MustCompile(`~~([^~]+)~~`)
	mdHeader      = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t]*`)
	mdQuote       = regexp.MustCompile(`(?m)^[ \t]*>+[ \t]?`)
	mdBullet      = regexp.MustCompile(`(?m)^[ \t]*[-*+][ \t]+`)
	mdRule        = regexp.MustCompile(`(?m)^[ \t]*(?:[-*_][ \t]*){3,}(?:\n|$)`)
	mdTableSep    = regexp.MustCompile(`(?m)^[ \t]*\|?[ \t]*:?-{2,}.*\|.*(?:\n|$)`)
	tableEdges    = regexp.MustCompile(`(?m)^[ \t]*\|[ \t]*|[ \t]*\|[ \t]*$`)
	tableInner    = regexp.MustCompile(`[ \t]*\|[ \t]*`)
	bareURL       = regexp.MustCompile(`https?://[^\s)\]>"']+`)
	lineEdges     = regexp.MustCompile(`(?m)^[ \t]+|[ \t]+$`)
	manyBlanks    = regexp.MustCompile(`\n{3,}`)
	manySpaces    = regexp.MustCompile(`[ \t]{2,}`)
)

// spokenText turns one reply into speakable prose: markdown markup goes,
// links become their text, a bare URL becomes its host, and code blocks —
// which no one wants read out token by token — are replaced with a short
// localized note.
func spokenText(text, language string) string {
	text = html.UnescapeString(text)

	text = fencedBlock.ReplaceAllString(text, codeOmitted(language))
	text = danglingFence.ReplaceAllString(text, codeOmitted(language))
	text = inlineCode.ReplaceAllString(text, "$1")

	text = mdImage.ReplaceAllString(text, "$1")
	text = mdLink.ReplaceAllString(text, "$1")
	text = bareURL.ReplaceAllStringFunc(text, spokenURL)

	text = mdTableSep.ReplaceAllString(text, "")
	text = tableEdges.ReplaceAllString(text, "")
	text = tableInner.ReplaceAllString(text, ", ")

	text = mdRule.ReplaceAllString(text, "")
	text = mdHeader.ReplaceAllString(text, "")
	text = mdQuote.ReplaceAllString(text, "")
	text = mdBullet.ReplaceAllString(text, "")

	text = mdBoldStars.ReplaceAllString(text, "$1")
	text = mdBoldUnder.ReplaceAllString(text, "$1")
	text = mdStrike.ReplaceAllString(text, "$1")
	// Whatever emphasis survived was unbalanced; none of these belong in
	// speech, and an underscore reads better as the space it stands for.
	text = strings.NewReplacer("*", "", "`", "", "~~", "", "_", " ").Replace(text)

	text = manySpaces.ReplaceAllString(text, " ")
	text = lineEdges.ReplaceAllString(text, "")
	text = manyBlanks.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}

// spokenURL says where a link goes, not how to type it.
func spokenURL(raw string) string {
	parsed, err := url.Parse(strings.TrimRight(raw, ".,;:"))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return strings.TrimPrefix(parsed.Host, "www.")
}

func codeOmitted(language string) string {
	if isSpanish(language) {
		return "(código omitido)"
	}
	return "(code omitted)"
}

// How a reply is cut into synthesis requests. A voice renders at a few times
// real time on a CPU, so the wait between a question and the first spoken
// word is set by how much text the first request carries — and the sentence
// the reply opens with is all the speakers need to start. The rest is
// rendered while that plays, a piece at a time, each piece allowed to be
// twice the one before it: a synthesizer keeping up at just twice real time
// has every piece in hand before the last one ends, and the gaps between
// pieces — one playback helper ends, the next spawns — fall on sentence
// ends, where a pause belongs.
const (
	// firstChunkChars is the least the first request carries. A reply that
	// opens with a word — "Sí." — is not worth a helper of its own and the
	// gap after it; the next sentence or two ride along.
	firstChunkChars = 24
	chunkGrowth     = 2
)

// sentenceEnd is a sentence's closing punctuation, any quote or bracket it is
// wrapped in, and the space after it. Punctuation with no space after it —
// a decimal, a version number, a domain — ends nothing.
var sentenceEnd = regexp.MustCompile(`[.!?…]+["'”’)\]]*\s+`)

// sentences cuts speakable prose at sentence ends and line breaks.
func sentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		last := 0
		for _, m := range sentenceEnd.FindAllStringIndex(line, -1) {
			if s := strings.TrimSpace(line[last:m[1]]); s != "" {
				out = append(out, s)
			}
			last = m[1]
		}
		if s := strings.TrimSpace(line[last:]); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// speechChunks groups a reply's sentences into synthesis requests: the first
// holds at least firstChunkChars, each later one up to chunkGrowth times the
// one before it and never past limit. A sentence is never split across two
// requests unless it alone is longer than limit.
func speechChunks(text string, limit int) []string {
	var chunks []string
	current := ""
	target := firstChunkChars
	flush := func() {
		if current == "" {
			return
		}
		chunks = append(chunks, current)
		target = min(limit, chunkGrowth*len(current))
		current = ""
	}
	for _, sentence := range sentences(text) {
		if len(sentence) > limit {
			flush()
			chunks = append(chunks, channel.SplitMessage(sentence, limit)...)
			target = limit
			continue
		}
		switch {
		case current == "":
			current = sentence
		case len(chunks) == 0, len(current)+1+len(sentence) <= target:
			// The first chunk is still short of its floor, or this one has
			// room under its cap.
			current += " " + sentence
		default:
			flush()
			current = sentence
		}
		if len(chunks) == 0 && len(current) >= firstChunkChars {
			flush()
		}
	}
	flush()
	if len(chunks) == 0 {
		return []string{text}
	}
	return chunks
}
