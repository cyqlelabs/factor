package voice

import (
	"html"
	"net/url"
	"regexp"
	"strings"
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
