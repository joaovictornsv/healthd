package notify

import (
	"os"
	"testing"
)

func TestSoundFile(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	n := New(Sounds{On: true, Files: map[Sound]string{SoundResolved: "/custom/done.mp3"}})
	if got := n.soundFile(SoundResolved); got != "/custom/done.mp3" {
		t.Errorf("configured sound = %q", got)
	}
	for _, s := range []Sound{SoundFiring, SoundCritical} {
		file := n.soundFile(s)
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s: built-in sound not extracted: %v", s, err)
		}
		want, _ := builtin.ReadFile(builtinFile[s])
		if string(data) != string(want) {
			t.Errorf("%s: extracted file differs from embedded one", s)
		}
	}
}
