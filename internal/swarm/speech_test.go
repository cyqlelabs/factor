package swarm

import "testing"

// The eval set for codes as they arrive: typed, pasted, and transcribed from
// speech in the languages Factor speaks. Every accepted case must land on the
// one code meant; every refused one could have meant two.
func TestNormalizeCodeEvalSet(t *testing.T) {
	accepted := map[string]string{
		"47-3829-1506":        "47-38291506",
		"47 · 3829 1506":      "47-38291506",
		"47 38291506":         "47-38291506",
		"4738291506":          "47-38291506",
		"  47-3829-1506.  ":   "47-38291506",
		"4 7 3 8 2 9 1 5 0 6": "47-38291506",
		"four seven, three eight two nine, one five oh six":                 "47-38291506",
		"Four seven dash three eight two nine dash one five zero six":       "47-38291506",
		"the code is 4 7 3 8 2 9 1 5 0 6":                                   "47-38291506",
		"cuatro siete, tres ocho dos nueve, uno cinco cero seis":            "47-38291506",
		"El código es cuatro siete tres ocho dos nueve uno cinco cero seis": "47-38291506",
		"Cuatro, siete. Tres, ocho, dos, nueve. Uno, cinco, cero, seis.":    "47-38291506",
		"uno 2 tres 4 cinco 6 siete 8 nueve":                                "1-23456789",
		"1-2345-6789":                                                       "1-23456789",
	}
	for in, want := range accepted {
		got, err := NormalizeCode(in)
		if err != nil || got != want {
			t.Errorf("NormalizeCode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	refused := []string{
		"forty seven 3829 1506",      // a number word: 47 or 40 7?
		"cuarenta y siete 3829 1506", // the same in Spanish
		"47 double five 29 1506",     // a repeat word
		"47-3829-150",                // a digit dropped from a group
		"47-382-91506",               // groups that do not split as a code does
		"47 3829150",                 // a password one digit short
		"uno dos tres",               // too short to be a code
		"1234567890123",              // too long
		"47-3829-15O6 please",        // a word that is no digit
		"",
	}
	for _, in := range refused {
		if got, err := NormalizeCode(in); err == nil {
			t.Errorf("NormalizeCode(%q) = %q; want it refused", in, got)
		}
	}
}

func TestCodeForms(t *testing.T) {
	if WrittenCode("47-38291506") != "47-3829-1506" {
		t.Error(WrittenCode("47-38291506"))
	}
	if SpokenCode("47-38291506") != "4 7, 3 8 2 9, 1 5 0 6" {
		t.Error(SpokenCode("47-38291506"))
	}
	if WrittenCode("junk") != "junk" || SpokenCode("junk") != "junk" {
		t.Error("an unreadable code is shown as it came")
	}
}
