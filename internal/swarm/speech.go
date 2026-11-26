package swarm

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Codes are digits because digits survive speech: a synthesized voice reads
// them reliably in every language Factor speaks, and a transcriber hears
// them well enough to be checked. What it cannot be trusted with is a
// number: "forty-seven" is two digits or three depending on what was meant,
// so only digit by digit is accepted, and anything else is refused rather
// than guessed at.

// digitWords are the spoken digits, in English and Spanish.
var digitWords = map[string]byte{
	"zero": '0', "oh": '0', "o": '0', "cero": '0',
	"one": '1', "uno": '1', "una": '1', "un": '1',
	"two": '2', "to": '2', "too": '2', "dos": '2',
	"three": '3', "tres": '3',
	"four": '4', "for": '4', "cuatro": '4',
	"five": '5', "cinco": '5',
	"six": '6', "seis": '6',
	"seven": '7', "siete": '7',
	"eight": '8', "ocho": '8',
	"nine": '9', "nueve": '9',
}

// fillers are the words a person says around the digits — or that a
// transcriber writes for a pause — and that carry none.
var fillers = map[string]bool{
	"dash": true, "guion": true, "guión": true, "space": true, "espacio": true,
	"the": true, "code": true, "is": true, "el": true, "código": true, "codigo": true, "es": true,
	"y": true, "and": true, "then": true, "luego": true,
}

// passwordDigits is how many digits after the mailbox number are the secret.
const passwordDigits = 8

// NormalizeCode turns a typed or transcribed code into its canonical form,
// "47-38291506": the mailbox number, then the eight secret digits. Spoken
// digits in English and Spanish are read as digits; a number word, a
// repeat word or anything else that could mean two things is refused.
func NormalizeCode(input string) (string, error) {
	var digits strings.Builder
	fields := strings.FieldsFunc(strings.ToLower(input), func(r rune) bool {
		return unicode.IsSpace(r) || r == '-' || r == '.' || r == ',' || r == '·' || r == '/' || r == ':'
	})
	for _, f := range fields {
		switch {
		case isDigits(f):
			digits.WriteString(f)
		case digitWords[f] != 0:
			digits.WriteByte(digitWords[f])
		case fillers[f]:
		default:
			return "", fmt.Errorf("%q is not a digit; say or type the code one digit at a time", f)
		}
	}
	// Typed in groups, the groups are checked: a digit dropped from one would
	// otherwise shift the split and read as another code.
	// Digits spaced one by one, as a transcriber writes them, are no groups.
	var groups []string
	grouped := false
	for _, f := range fields {
		if isDigits(f) {
			groups = append(groups, f)
			grouped = grouped || len(f) > 1
		}
	}
	if grouped && len(groups) == len(fields) && len(groups) >= 2 {
		tail := strings.Join(groups[1:], "")
		if len(groups) == 2 && len(tail) != passwordDigits ||
			len(groups) >= 3 && (len(groups[len(groups)-1]) != 4 || len(groups[len(groups)-2]) != 4) {
			return "", errors.New("a code is written as a number and two groups of four digits; this one is not")
		}
	}
	d := digits.String()
	if len(d) <= passwordDigits {
		return "", fmt.Errorf("a code has more than %d digits; this one has %d", passwordDigits, len(d))
	}
	if len(d) > passwordDigits+4 {
		return "", errors.New("that is too many digits for a code")
	}
	return d[:len(d)-passwordDigits] + "-" + d[len(d)-passwordDigits:], nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// codeParts splits a canonical code.
func codeParts(code string) (nameplate, password string, err error) {
	norm, err := NormalizeCode(code)
	if err != nil {
		return "", "", err
	}
	nameplate, password, _ = strings.Cut(norm, "-")
	return nameplate, password, nil
}

// WrittenCode is how a code is shown: 47-3829-1506.
func WrittenCode(code string) string {
	n, p, err := codeParts(code)
	if err != nil {
		return code
	}
	return n + "-" + p[:4] + "-" + p[4:]
}

// SpokenCode is how a code is read aloud: digit by digit, the groups apart,
// so the voice says digits and never a number.
func SpokenCode(code string) string {
	n, p, err := codeParts(code)
	if err != nil {
		return code
	}
	spaced := func(s string) string { return strings.Join(strings.Split(s, ""), " ") }
	return spaced(n) + ", " + spaced(p[:4]) + ", " + spaced(p[4:])
}
