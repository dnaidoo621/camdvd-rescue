package web

import (
	"net/http"

	"github.com/dnaidoo621/camdvd-rescue/internal/jobs"
	"github.com/dnaidoo621/camdvd-rescue/internal/library"
	"github.com/dnaidoo621/camdvd-rescue/internal/store"
)

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.E.Overview())
}

func (s *Server) apiDrives(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.E.Drives())
}

func (s *Server) apiEject(w http.ResponseWriter, r *http.Request) {
	if err := s.E.Eject(r.PathValue("id")); err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) apiInsert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Image string `json:"image"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, err)
		return
	}
	if err := s.E.Insert(r.PathValue("id"), req.Image); err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// JobView is a disc job with its clips, as the API returns it.
type JobView struct {
	*store.Disc
	Clips []*store.Clip `json:"clips"`
}

func (s *Server) job(id string) (*JobView, error) {
	d, err := s.E.St.Disc(id)
	if err != nil {
		return nil, err
	}
	clips, err := s.E.St.Clips(id)
	if err != nil {
		return nil, err
	}
	return &JobView{d, clips}, nil
}

func (s *Server) apiJobs(w http.ResponseWriter, r *http.Request) {
	var states []store.State
	if st := r.URL.Query().Get("state"); st != "" {
		states = append(states, store.State(st))
	}
	discs, err := s.E.St.Discs(states...)
	if err != nil {
		httpError(w, err)
		return
	}
	if discs == nil {
		discs = []*store.Disc{}
	}
	writeJSON(w, discs)
}

func (s *Server) apiJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.job(r.PathValue("id"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, j)
}

func (s *Server) apiAnswers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sides       int    `json:"sides"`
		Description string `json:"description"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, err)
		return
	}
	d, err := s.E.Answers(r.PathValue("id"), req.Sides, req.Description)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, d)
}

// jobAction runs a named action on a job.
func (s *Server) jobAction(id, action string) error {
	switch action {
	case "cancel":
		return s.E.Cancel(id)
	case "resume":
		return s.E.Resume(id)
	case "force-raw":
		return s.E.ForceRaw(id)
	case "reprocess":
		return s.E.Reprocess(id)
	case "import-again":
		return s.E.ConfirmDuplicate(id)
	case "add-side":
		return s.E.AddSide(id)
	case "delete":
		return s.E.DeleteJob(id)
	}
	return errUnknownAction
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errUnknownAction = constErr("unknown action")

func (s *Server) apiJobAction(w http.ResponseWriter, r *http.Request) {
	if err := s.jobAction(r.PathValue("id"), r.PathValue("action")); err != nil {
		if err == errUnknownAction {
			http.NotFound(w, r)
			return
		}
		httpError(w, err)
		return
	}
	j, err := s.job(r.PathValue("id"))
	if err != nil {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, j)
}

func (s *Server) apiDeleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.E.DeleteJob(r.PathValue("id")); err != nil {
		httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiLibrary(w http.ResponseWriter, r *http.Request) {
	lib, err := s.E.Library()
	if err != nil {
		httpError(w, err)
		return
	}
	if lib == nil {
		lib = []jobs.Folder{}
	}
	writeJSON(w, lib)
}

func (s *Server) apiPatchFolder(w http.ResponseWriter, r *http.Request) {
	var ed jobs.DiscEdit
	if err := readJSON(r, &ed); err != nil {
		httpError(w, err)
		return
	}
	if err := s.E.EditDisc(r.Context(), r.PathValue("id"), ed); err != nil {
		httpError(w, err)
		return
	}
	f, err := s.E.Folder(r.PathValue("id"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, f)
}

func (s *Server) apiRescan(w http.ResponseWriter, r *http.Request) {
	rep, err := s.E.Rescan(r.Context())
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, rep)
}

func (s *Server) apiRenamePreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Selection jobs.Selection  `json:"selection"`
		Pattern   library.Pattern `json:"pattern"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, err)
		return
	}
	pv, err := s.E.PreviewRename(req.Selection, req.Pattern)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, pv)
}

func (s *Server) apiRenameApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreviewID string `json:"preview_id"`
	}
	if err := readJSON(r, &req); err != nil {
		httpError(w, err)
		return
	}
	b, err := s.E.ApplyRename(req.PreviewID)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, b)
}

func (s *Server) apiRenameUndo(w http.ResponseWriter, r *http.Request) {
	if err := s.E.UndoRename(r.PathValue("batchId")); err != nil {
		httpError(w, err)
		return
	}
	b, _ := s.E.St.Batch(r.PathValue("batchId"))
	writeJSON(w, b)
}

func (s *Server) apiBatches(w http.ResponseWriter, r *http.Request) {
	bs, err := s.E.St.Batches(50)
	if err != nil {
		httpError(w, err)
		return
	}
	if bs == nil {
		bs = []*store.RenameBatch{}
	}
	writeJSON(w, bs)
}

func (s *Server) apiDoctor(w http.ResponseWriter, r *http.Request) {
	rep := s.Doctor(r.Context())
	s.SetReport(rep)
	if !rep.OK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	writeJSON(w, rep)
}
