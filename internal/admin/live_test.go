package admin

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOnlineSavePersistsAppliesAndTracksPendingRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.conf")
	original := []byte("noise-interval = 0\nnoise-min-bytes = 1024\nnoise-max-bytes = 10240\ntarget = localhost:1\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	var keys []string
	var restarts int
	var fail bool
	h := &Handler{view: View{AppConfigPath: path, LiveFields: []string{"noise-interval", "noise-min-bytes", "noise-max-bytes"}, ApplyLive: func(k []string) error {
		if fail {
			return errors.New("injected failure")
		}
		keys = k
		return nil
	}, Apply: func() error { restarts++; return nil }}}
	h.startupRevision = h.restartDigest(original)
	hot := "noise-interval = 1s\nnoise-min-bytes = 1024\nnoise-max-bytes = 10240\ntarget = localhost:1\n"
	w := httptest.NewRecorder()
	h.commitConfig(w, original, hot, true)
	if w.Code != 200 || restarts != 0 || !reflect.DeepEqual(keys, []string{"noise-interval"}) {
		t.Fatal(w.Code, w.Body.String(), keys, restarts)
	}
	data, _ := os.ReadFile(path)
	if string(data) != hot || h.restartDigest(data) != h.startupRevision {
		t.Fatal("hot save was not persisted/applied")
	}
	pending := hot + "password = pending-password\n"
	w = httptest.NewRecorder()
	h.commitConfig(w, data, pending, false)
	if w.Code != 200 || h.restartDigest([]byte(pending)) == h.startupRevision {
		t.Fatal("cold setting lost pending restart")
	}
	updated := "noise-interval = 2s\nnoise-min-bytes = 1024\nnoise-max-bytes = 10240\ntarget = localhost:1\npassword = pending-password\n"
	fail = true
	w = httptest.NewRecorder()
	h.commitConfig(w, []byte(pending), updated, false)
	data, _ = os.ReadFile(path)
	if w.Code != 409 || string(data) != pending {
		t.Fatal("hot apply failure did not restore file", w.Code)
	}
	fail = false
	w = httptest.NewRecorder()
	h.commitConfig(w, data, updated, false)
	if w.Code != 200 || h.restartDigest([]byte(updated)) == h.startupRevision {
		t.Fatal("hot save cleared pending cold setting")
	}
	w = httptest.NewRecorder()
	h.commitConfig(w, []byte(updated), updated, true)
	if w.Code != 200 || restarts != 1 || h.restartDigest([]byte(updated)) != h.startupRevision {
		t.Fatal("cold apply did not restart")
	}
}

func TestRemovedOnlineFieldRestoresDefaultsWithoutClearingPendingChanges(t *testing.T) {
	h := &Handler{view: View{LiveFields: []string{"noise-interval"}, ApplyLive: func([]string) error { return nil }}}
	before := []byte("noise-interval = 1s\ntarget = localhost:1\n")
	after := "target = localhost:1\n"
	if got := h.changedLiveFields(before, after); !reflect.DeepEqual(got, []string{"noise-interval"}) {
		t.Fatal(got)
	}
	if h.restartDigest(before) != h.restartDigest([]byte(after)) {
		t.Fatal("removed live field incorrectly requires restart")
	}
}
