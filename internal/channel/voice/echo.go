package voice

import (
	"strings"
	"sync"
	"time"
)

// The echo tracker is the transcript-level half of feedback control. The
// segmenter's raised barge bar keeps most of the speakers' sound out of the
// microphone; when the volume is high enough that it gets in anyway, what got
// in is the agent's own voice — and its words are known, because this channel
// synthesized them moments ago. An utterance the agent was speaking during is
// matched against what was recently sent to the speakers: one that is nothing
// else is discarded as feedback instead of becoming a turn the agent holds
// with itself, and one that carries the user's words as well keeps only those.

const (
	// echoMemoryWords bounds what the tracker remembers — enough to cover the
	// longest reply still coming out of the speakers.
	echoMemoryWords = 400

	// echoMinWords is the shortest matched run that counts as echo: below it,
	// a coincidence — the user saying "stop" while the reply contains "stop" —
	// would swallow a real command.
	echoMinWords = 3

	// echoDiscardWords is the higher bar for throwing an utterance away
	// whole. A short barge that quotes the reply — "turn the music off" over
	// "I can turn the music off if you like" — matches end to end, and there
	// is no wake word in barge mode to rescue it; only a run long enough that
	// no one dictates it as a command is written off as pure feedback.
	echoDiscardWords = 5

	// echoGap is how many spoken words a match may skip past, absorbing the
	// words the microphone missed on their way back in.
	echoGap = 4
)

type echoTracker struct {
	mu    sync.Mutex
	words []string // normalized words recently sent to the speakers, oldest first
	// until is when those words stop counting as echo; zero while a reply is
	// still being spoken.
	until time.Time
}

// record remembers text on its way to the speakers.
func (e *echoTracker) record(text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.until = time.Time{}
	for _, f := range strings.Fields(text) {
		if n := normalizeWord(f); n != "" {
			e.words = append(e.words, n)
		}
	}
	if over := len(e.words) - echoMemoryWords; over > 0 {
		e.words = append([]string(nil), e.words[over:]...)
	}
}

// expire sets how long a reply that has been heard out stays matchable.
// It is deliberately not an immediate forget: an utterance that captured the
// tail of a reply does not close until silence_ms of quiet has passed, and is
// only transcribed after that, so clearing the moment the speakers fall
// silent throws the words away a beat before the echo carrying them arrives.
// Past the delay they are forgotten, because audio no longer in the air
// cannot echo and remembering it would swallow a later barge that quotes it.
func (e *echoTracker) expire(after time.Duration) {
	e.mu.Lock()
	e.until = time.Now().Add(after)
	e.mu.Unlock()
}

// recall is what the speakers said that can still be in the air.
func (e *echoTracker) recall() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.until.IsZero() && !time.Now().Before(e.until) {
		e.words, e.until = nil, time.Time{}
	}
	// Handing the slice header back without copying is safe only because
	// record never writes into its range: appends land past its length, and
	// the truncation copies to a fresh array.
	return e.words
}

// strip removes the agent's own recent words from a transcript captured while
// the speakers were talking, wherever in that transcript they fell. It returns
// what is left, and whether the utterance was nothing else.
//
// The echo is not always the prefix. A user who begins a sentence in the pause
// before the reply lands still has the microphone segment open when the
// speakers start, so what comes back is the user, then the agent, then the
// user again — the agent's words in the middle of the user's.
func (e *echoTracker) strip(text string) (string, bool) {
	tokens := tokenizeWords(text)
	spoken := e.recall()
	if len(tokens) == 0 || len(spoken) == 0 {
		return text, false
	}
	echoed := make([]bool, len(tokens))
	matched, removed := 0, 0
	for i := 0; i < len(tokens); {
		run, hits := echoRun(tokens[i:], spoken)
		if hits < echoMinWords {
			i++
			continue
		}
		for j := i; j < i+run; j++ {
			echoed[j] = true
		}
		matched, removed = matched+hits, removed+run
		i += run
	}
	if removed == 0 {
		return text, false
	}
	if removed == len(tokens) {
		if matched >= echoDiscardWords {
			return "", true
		}
		return text, false // short enough to be a quoted command — keep it
	}
	rest := keptWords(text, tokens, echoed)
	if rest == "" {
		return "", true
	}
	return rest, false
}

// echoRun reports how far one aligned run of spoken words reaches from the
// front of tokens — the count past its last aligned token, and how many
// actually matched. The alignment may start anywhere in the record, skip up
// to echoGap spoken words between matches, and absorb lone mis-transcribed
// tokens; two consecutive unmatched tokens end it — that is where the run
// stops and somebody else's words begin.
func echoRun(tokens []wordToken, spoken []string) (int, int) {
	best, bestMatched := 0, 0
	for start, w := range spoken {
		if w != tokens[0].norm {
			continue
		}
		matched, run := 1, 1
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
			run = i + 1
		}
		if matched >= echoMinWords && run > best {
			best, bestMatched = run, matched
		}
	}
	return best, bestMatched
}

// keptWords rebuilds the transcript from the words that were not echo, one
// contiguous run at a time, so the user's own spelling and punctuation
// survive the removal.
func keptWords(text string, tokens []wordToken, echoed []bool) string {
	var b strings.Builder
	for i := 0; i < len(tokens); {
		if echoed[i] {
			i++
			continue
		}
		j := i
		for j < len(tokens) && !echoed[j] {
			j++
		}
		// Leading punctuation is what the removal left behind — the comma
		// that followed the agent's last word. Trailing punctuation is the
		// user's own sentence ending, and stays.
		span := strings.TrimSpace(text[tokens[i].start:tokens[j-1].end])
		span = strings.TrimSpace(strings.TrimLeft(span, " \t\n,.;:!?¡¿-—"))
		if span != "" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(span)
		}
		i = j
	}
	return b.String()
}

// wordToken is one word of a transcript, normalized, with the byte offsets
// bounding it in the original text — so a match can hand back exactly the
// words around it.
type wordToken struct {
	norm  string
	start int
	end   int
}

func tokenizeWords(text string) []wordToken {
	var tokens []wordToken
	start := -1
	for i, r := range text {
		if r == ' ' || r == '\t' || r == '\n' {
			if start >= 0 {
				tokens = append(tokens, wordToken{normalizeWord(text[start:i]), start, i})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		tokens = append(tokens, wordToken{normalizeWord(text[start:]), start, len(text)})
	}
	return tokens
}
