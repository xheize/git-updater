package tracker

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type JobStatus string

const (
	StatusPending JobStatus = "Pending"
	StatusRunning JobStatus = "Running"
	StatusSuccess JobStatus = "Success"
	StatusFailed  JobStatus = "Failed"
)

type JobRecord struct {
	ID        string    `json:"id"`
	File      string    `json:"file"`
	Image     string    `json:"image"`
	Tag       string    `json:"tag"`
	Status    JobStatus `json:"status"`
	Error     string    `json:"error,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type Tracker struct {
	mu         sync.RWMutex
	filePath   string
	Jobs       []JobRecord `json:"jobs"`
	AutoUpdate bool        `json:"autoUpdate"`
}

func New(filePath string, defaultAutoUpdate bool) *Tracker {
	t := &Tracker{
		filePath:   filePath,
		Jobs:       make([]JobRecord, 0),
		AutoUpdate: defaultAutoUpdate,
	}
	t.Load()
	return t
}

func (t *Tracker) Load() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	data, err := os.ReadFile(t.filePath)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, t)
}

func (t *Tracker) Save() error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(t.filePath, data, 0644)
}

func (t *Tracker) AddJob(id, file, image, tag string, status JobStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Avoid duplicates
	for i, j := range t.Jobs {
		if j.ID == id {
			t.Jobs[i].Status = status
			t.Jobs[i].Timestamp = time.Now()
			t.Save()
			return
		}
	}

	record := JobRecord{
		ID:        id,
		File:      file,
		Image:     image,
		Tag:       tag,
		Status:    status,
		Timestamp: time.Now(),
	}
	t.Jobs = append([]JobRecord{record}, t.Jobs...) // Newest first
	if len(t.Jobs) > 200 { // Limit history size
		t.Jobs = t.Jobs[:200]
	}
	t.Save()
}

func (t *Tracker) UpdateJobStatus(id string, status JobStatus, errMsg string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i, j := range t.Jobs {
		if j.ID == id {
			t.Jobs[i].Status = status
			t.Jobs[i].Error = errMsg
			t.Jobs[i].Timestamp = time.Now()
			t.Save()
			break
		}
	}
}

func (t *Tracker) SetAutoUpdate(enabled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.AutoUpdate = enabled
	t.Save()
}

func (t *Tracker) IsAutoUpdate() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.AutoUpdate
}

func (t *Tracker) GetJobs() []JobRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()

	jobs := make([]JobRecord, len(t.Jobs))
	copy(jobs, t.Jobs)
	return jobs
}

func (t *Tracker) ClearHistory() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.Jobs = make([]JobRecord, 0)
	t.Save()
}
