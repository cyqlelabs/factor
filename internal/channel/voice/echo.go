package voice

import (
	"strings"
	"sync"
)

// The echo tracker is the transcript-level half of feedback control. The
// segmenter's raised barge bar keeps most of the speakers' sound out of the
// microphone; when the volume is high enough that it gets in anyway, what got
// in is the agent's own voice — and its words are known, because this channel
// synthesized them moments ago. A barged utterance is matched against what was
// recently sent to the speakers: one that is nothing else is discarded as
// feedback instead of becoming a turn the agent holds with itself, and one
// that carries the user's words behind the echo keeps only those.

const (
	// echoMemoryWords bounds what the tracker remembers — enough to cover the
	// longest reply still coming out of the speakers.
	echoMemoryWords = 400

	// echoMinWords is the shortest matched run that counts as echo: below it,
	// a coincidence — the user saying "stop" while the reply contains "stop" —
	// would swallow a real command.
	echoMinWords = 3

	// echoGap is how many spoken words a match may skip past, absorbing the
	// words the microphone missed on their way back in.
	echoGap = 4
)

type echoTracker struct {
	mu    sync.Mutex
	words []string // normalized words recently sent to the speakers, oldest first
}

// record remembers text on its way to the speakers.
func (e *echoTracker) record(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, f := range strings.Fields(text) {
		if n := normalizeWord(f); n != "" {
			e.words = append(e.words, n)
		}
	}
	if over := len(e.words) - echoMemoryWords; over > 0 {
		e.words = append([]string(nil), e.words[over:]...)
	}
}

// strip removes the agent's own recent words from the front of a barged
// transcript — the speakers were talking first, so the echo is always the
// prefix. It returns what remains, and whether the utterance was nothing else.
func (e *echoTracker) strip(text string) (string, bool) {
	tokens := tokenizeWords(text)
	if len(tokens) == 0 {
		return text, false
	}
	e.mu.Lock()
	spoken := e.words
	e.mu.Unlock()
	matched := echoPrefix(tokens, spoken)
	if matched == 0 {
		return text, false
	}
	if matched == len(tokens) {
		return "", true
	}
	rest := strings.TrimLeft(text[tokens[matched-1].end:], " \t\n,.;:!?¡¿-—")
	return strings.TrimSpace(rest), false
}

// echoPrefix reports how many leading tokens align, in order, with the spoken
// words. The alignment may start anywhere in the record, skip up to echoGap
// spoken words between matches, and absorb lone mis-transcribed tokens; two
// consecutive unmatched tokens end it — that is where the user starts.
func echoPrefix(tokens []wordToken, spoken []string) int {
	best := 0
	for start, w := range spoken {
		if w != tokens[0].norm {
			continue
		}
		matched, prefix := 1, 1
		j, misses := start+1, 0
		for i := 1; i < len(tokens); i++ {
			found := -1
			for k := j; k < len(spoken) && k < j+echoGap; k++ {
				if spoken[k] == tokens[i].norm {
					found = k
					break
				}
			}
			if found < 0 {
				if misses++; misses >= 2 {
					break
				}
				continue
			}
			j, misses = found+1, 0
			matched++
			prefix = i + 1
		}
		if matched >= echoMinWords && prefix > best {
			best = prefix
		}
	}
	return best
}

// wordToken is one word of a transcript, normalized, with the byte offset just
// past it in the original text — so a match can hand back exactly what follows.
type wordToken struct {
	norm string
	end  int
}

func tokenizeWords(text string) []wordToken {
	var tokens []wordToken
	start := -1
	for i, r := range text {
		if r == ' ' || r == '\t' || r == '\n' {
			if start >= 0 {
				tokens = append(tokens, wordToken{normalizeWord(text[start:i]), i})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		tokens = append(tokens, wordToken{normalizeWord(text[start:]), len(text)})
	}
	return tokens
}
