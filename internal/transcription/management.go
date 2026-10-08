package transcription

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"tgcontrol/internal/agentdesk"
	"time"
)

type Model struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	Description string  `json:"description,omitempty"`
	Engine      string  `json:"engine,omitempty"`
	DiskGB      float64 `json:"disk_gb"`
	VRAMGB      float64 `json:"vram_gb"`
	RAMGB       float64 `json:"ram_gb"`
	Cached      bool    `json:"cached"`
	Ready       bool    `json:"ready"`
	Supported   bool    `json:"supported"`
	RussianOnly bool    `json:"russian_only"`
	GPUOnly     bool    `json:"gpu_only"`
}
type DictionaryEntry struct {
	Heard   string `json:"heard"`
	Written string `json:"written"`
}
type Preferences struct {
	Model      string            `json:"model"`
	Language   string            `json:"language"`
	Dictionary []DictionaryEntry `json:"dictionary"`
}
type Details struct {
	Management    bool        `json:"management"`
	ModuleVersion string      `json:"module_version"`
	Models        []Model     `json:"models"`
	Settings      Preferences `json:"settings"`
	Hardware      struct {
		Name             string `json:"name"`
		HasNvidia        bool   `json:"has_nvidia"`
		MemoryMB         int    `json:"memory_mb"`
		RecommendedModel string `json:"recommended_model"`
	} `json:"hardware"`
}
type Action struct {
	Operation string       `json:"operation"`
	ID        string       `json:"id"`
	Model     string       `json:"model,omitempty"`
	Settings  *Preferences `json:"settings,omitempty"`
}

func validJobID(id string) bool {
	value, err := hex.DecodeString(id)
	return err == nil && len(value) == 16
}

func (s *Service) workerLocked(uid int64, exe string) (Worker, error) {
	if s.idle != nil {
		s.idle.Stop()
	}
	s.warmRevision++
	if s.worker != nil && s.workerUID != uid {
		s.releaseWorkerLocked()
	}
	if s.worker == nil {
		progress := &speechProgress{}
		worker, err := s.newWorker(agentdesk.Config{Executable: exe, DataDir: filepath.Join(s.root, "users", strconv.FormatInt(uid, 10)), AllowedRoots: []string{s.uploadsRoot}, RequestTimeout: 30 * time.Minute, Stderr: progress})
		if err != nil {
			return nil, err
		}
		s.worker, s.workerUID = worker, uid
		s.speech = progress
	}
	return s.worker, nil
}

// Management shares the same single-job gate, ownership and cancel protocol as ASR.
func (s *Service) StartAction(uid int64, action Action) (Job, error) {
	if action.Operation == "module.install" {
		return s.startModuleInstall(uid, action.ID)
	}
	method := map[string]string{"inspect": "local.inspect", "install": "local.model.install", "save": "local.settings.save"}[action.Operation]
	if method == "" {
		return Job{}, errors.New("неизвестное действие локального модуля")
	}
	if action.Operation == "save" && action.Settings == nil {
		return Job{}, errors.New("укажите параметры распознавания")
	}
	if action.Operation == "install" && action.Model == "" {
		return Job{}, errors.New("выберите модель")
	}
	if !validJobID(action.ID) {
		return Job{}, errors.New("неверный идентификатор задачи")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if previous := s.jobs[action.ID]; previous != nil {
		if previous.uid != uid {
			return Job{}, ErrNotFound
		}
		return *previous, nil
	}
	if s.closing {
		return Job{}, errors.New("распознавание остановлено")
	}
	if s.active != nil {
		return Job{}, ErrBusy
	}
	exe := s.executableLocked()
	if exe == "" {
		return Job{}, errors.New("подключите модуль на выбранном компьютере")
	}
	worker, err := s.workerLocked(uid, exe)
	if err != nil {
		return Job{}, err
	}
	timeout := 30 * time.Second
	if action.Operation == "install" {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	job := &Job{ID: action.ID, Kind: action.Operation, State: "running", uid: uid, cancel: cancel}
	s.jobs[job.ID], s.active = job, job
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		params := any(map[string]string{"model": action.Model})
		if action.Operation == "save" {
			params = action.Settings
		}
		var result Details
		err := worker.Call(ctx, method, params, &result)
		var remote *agentdesk.RemoteError
		if action.Operation == "inspect" && errors.As(err, &remote) && remote.Code == "method_not_found" {
			result, err = legacyDetails(ctx, worker)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if job.State != "cancelled" {
			if err != nil {
				job.State, job.Error = "failed", err.Error()
			} else {
				job.State = "done"
				job.Result, _ = json.Marshal(result)
			}
		}
		job.finished = time.Now()
		s.active = nil
		s.armIdleLocked()
	}()
	return *job, nil
}

func legacyDetails(ctx context.Context, worker Worker) (Details, error) {
	var caps struct {
		Models  []string `json:"models"`
		Catalog []Model  `json:"model_catalog"`
		Default string   `json:"default_model"`
	}
	if err := worker.Call(ctx, "system.capabilities", nil, &caps); err != nil {
		return Details{}, err
	}
	result := Details{ModuleVersion: "legacy", Models: caps.Catalog, Settings: Preferences{Model: caps.Default, Language: "ru", Dictionary: []DictionaryEntry{}}}
	for i := range result.Models {
		result.Models[i].Supported = true
	}
	if len(result.Models) == 0 {
		for _, id := range caps.Models {
			result.Models = append(result.Models, Model{ID: id, Label: id, Supported: true})
		}
	}
	if result.Settings.Model == "" && len(result.Models) > 0 {
		result.Settings.Model = result.Models[0].ID
	}
	return result, nil
}
