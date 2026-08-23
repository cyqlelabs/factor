package voice

import "testing"

func TestNoiseOnlyRecognizesWhatNobodySaid(t *testing.T) {
	noise := []string{
		"Gracias por ver el video.",
		"gracias por ver el video",
		"¡Suscríbete al canal!",
		"Suscribete al canal.",
		// The doubled form the decoder falls into on a long silence.
		"¡Suscríbete al canal!  ¡Suscríbete al canal!",
		"Thanks for watching!",
		"Subtitles by the Amara.org community",
		"Subtítulos realizados por la comunidad de Amara.org",
	}
	for _, text := range noise {
		if !noiseOnly(text) {
			t.Errorf("%q was not recognized as transcription noise", text)
		}
	}

	speech := []string{
		// The reason a bare "gracias" is not in the table: it is among the
		// most common things a person actually says.
		"Gracias.",
		"Thank you!",
		"Gracias por el dato, Factor.",
		"Factor, abrí la página de lanación.com.",
		"No, en github.com, donde están los repositorios.",
		// One noise phrase inside a real sentence is a person quoting a
		// television, not a television.
		"El video decía gracias por ver el video y se cortó.",
		"",
		"   ",
	}
	for _, text := range speech {
		if noiseOnly(text) {
			t.Errorf("%q was thrown away as transcription noise", text)
		}
	}
}

func TestNormalizeNoiseFoldsAccentsAndPunctuation(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"¡Suscríbete al canal!", "suscribete al canal"},
		{"  Amara.org  ", "amara org"},
		{"ÑOÑO", "nono"},
		{"...", ""},
	} {
		if got := normalizeNoise(tc.in); got != tc.want {
			t.Errorf("normalizeNoise(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGateTreatsASubtitleCreditAsNoise(t *testing.T) {
	t.Setenv("FACTOR_HOME", t.TempDir())
	cfg := validConfig()
	cfg.Activation = "always"
	v, err := New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dec := v.gateNow("Gracias por ver el video.", false); dec.accept || !dec.noise {
		t.Errorf("a subtitle credit was not judged noise: %+v", dec)
	}
	// An empty transcript is the same finding by a different route: the
	// transcriber heard no person either way.
	if dec := v.gateNow("   ", false); dec.accept || !dec.noise {
		t.Errorf("an empty transcript was not judged noise: %+v", dec)
	}
	if dec := v.gateNow("Gracias, Factor.", false); !dec.accept || dec.noise {
		t.Errorf("a real sentence was judged noise: %+v", dec)
	}
}
