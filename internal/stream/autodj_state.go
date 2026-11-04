package stream

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"
)

// autoDJStateFile records the track the AutoDJ is playing, in the studio's
// audio directory, so a restart resumes at that track instead of always
// starting from the top of the playlist.
const autoDJStateFile = "autodj-state.json"

type autoDJState struct {
	TrackID string    `json:"track_id"`
	SavedAt time.Time `json:"saved_at"`
}

func (a *autoDJ) statePath() string {
	return filepath.Join(a.dir, autoDJStateFile)
}

func (a *autoDJ) loadState() (autoDJState, bool) {
	data, err := os.ReadFile(a.statePath())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("autoDJ: reading state: %v", err)
		}
		return autoDJState{}, false
	}
	var st autoDJState
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("autoDJ: parsing state %s: %v", a.statePath(), err)
		return autoDJState{}, false
	}
	if st.TrackID == "" {
		return autoDJState{}, false
	}
	return st, true
}

// saveState records t as the track on air. It writes a temp file and renames
// it over the old one, so a crash mid-write never leaves a corrupt state file.
// Failures are logged and otherwise ignored: losing the resume point must not
// interrupt playback. Tracks without a backend ID can't be resumed, so they
// leave the previous resume point in place.
func (a *autoDJ) saveState(t Track) {
	if t.ID == "" {
		return
	}
	// Marshalling a string and a time can't fail.
	data, _ := json.Marshal(autoDJState{TrackID: t.ID, SavedAt: time.Now().UTC()})
	tmp, err := os.CreateTemp(a.dir, autoDJStateFile+".*.tmp")
	if err != nil {
		log.Printf("autoDJ: saving state: %v", err)
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		log.Printf("autoDJ: saving state: %v", err)
		return
	}
	if err := os.Rename(tmp.Name(), a.statePath()); err != nil {
		os.Remove(tmp.Name())
		log.Printf("autoDJ: saving state: %v", err)
	}
}

// resume points the playlist at the saved track, once per process. It waits
// for the playlist to load (the backend may be down at boot) before giving up
// on the saved track, so it returns false without spending its one attempt
// while the playlist is still empty.
func (a *autoDJ) resume() (Track, bool) {
	if _, loaded := a.playlist.nextTrack(); !loaded {
		return Track{}, false
	}
	a.resumeTried = true

	st, ok := a.loadState()
	if !ok {
		return Track{}, false
	}
	t, found := a.playlist.seek(func(t Track) bool { return t.ID == st.TrackID })
	if !found {
		log.Printf("autoDJ: saved track %s is no longer in the playlist; starting from the top", st.TrackID)
		return Track{}, false
	}
	log.Printf("autoDJ: resuming at %s (saved %s)", t.Title, st.SavedAt.Format(time.RFC3339))
	return t, true
}
