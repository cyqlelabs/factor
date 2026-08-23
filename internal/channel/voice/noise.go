package voice

import (
	"strings"
	"unicode"
)

// Transcription noise: what comes back from the transcriber that nobody said.
//
// The managed speech server already drops what its decoder was unsure of —
// no_speech_prob and avg_logprob, in speechserver.py — which catches the
// mumbling read out of line noise. It does not catch the other family, the
// one Whisper emits with every confidence score healthy because its training
// data is full of it: the credits that close a subtitled video. A television
// in the room comes back as "Gracias por ver el video.", and on this machine
// that has already been answered as a turn.
//
// Two rules keep the table from eating real speech. The phrase has to be the
// whole utterance, so "gracias" inside a sentence is somebody being polite;
// and the table holds only lines nobody says to an agent. A bare "gracias" or
// "thank you" is deliberately absent — it is among the most common things a
// person actually says, and dropping it to catch a hallucination trades a
// rare phantom turn for a frequent deaf one.
var noisePhrases = map[string]bool{
	"gracias por ver el video":                            true,
	"gracias por ver este video":                          true,
	"suscribete al canal":                                 true,
	"no te olvides de suscribirte":                        true,
	"subtitulos realizados por la comunidad de amara org": true,
	"mas informacion en www astromia com":                 true,
	"thanks for watching":                                 true,
	"thank you for watching":                              true,
	"thanks for watching this video":                      true,
	"please subscribe":                                    true,
	"subscribe to my channel":                             true,
	"like and subscribe":                                  true,
	"subtitles by the amara org community":                true,
}

// noiseOnly reports whether an utterance is nothing but phrases from the
// table. The whole utterance has to be consumed by them, which is what
// recognizes the doubled form the decoder falls into — "¡Suscríbete al canal!
// ¡Suscríbete al canal!" — without a rule about repetition, and what keeps a
// person quoting one of these lines mid-sentence from being thrown away.
func noiseOnly(text string) bool {
	words := strings.Fields(normalizeNoise(text))
	if len(words) == 0 {
		return false
	}
	for i := 0; i < len(words); {
		matched := 0
		for n := min(maxNoiseWords, len(words)-i); n >= 1; n-- {
			if noisePhrases[strings.Join(words[i:i+n], " ")] {
				matched = n
				break
			}
		}
		if matched == 0 {
			return false
		}
		i += matched
	}
	return true
}

// maxNoiseWords is the longest phrase in the table, in words: how far ahead
// a match has to look.
var maxNoiseWords = func() int {
	longest := 0
	for phrase := range noisePhrases {
		if n := len(strings.Fields(phrase)); n > longest {
			longest = n
		}
	}
	return longest
}()

// normalizeNoise reduces one sentence to the form the table is written in:
// lower case, the Spanish accents folded away because a transcriber places
// them inconsistently, and everything that is not a letter, a digit or a
// single space removed.
func normalizeNoise(sentence string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(sentence) {
		if folded, ok := accentFolds[r]; ok {
			r = folded
		}
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}

var accentFolds = map[rune]rune{
	'á': 'a', 'é': 'e', 'í': 'i', 'ó': 'o', 'ú': 'u', 'ü': 'u', 'ñ': 'n',
	'à': 'a', 'è': 'e', 'ì': 'i', 'ò': 'o', 'ù': 'u', 'ç': 'c',
}
