package app

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestVideoJobReadIsSnapshot(t *testing.T) {
	j := &videoJob{ID: "video_snapshot", Status: "queued", CreatedAt: time.Now().Unix()}
	putVideoJob(j)
	snapshot := getVideoJob(j.ID)
	videoJobsMu.Lock()
	j.Status = "completed"
	j.Error = "worker result"
	videoJobsMu.Unlock()
	if snapshot.Status != "queued" || snapshot.Error != "" {
		t.Fatal("response snapshot changed with worker state")
	}
	if getVideoJob(j.ID).Status != "completed" {
		t.Fatal("fresh read missing worker state")
	}
	if getVideoJob("video_missing_snapshot") != nil {
		t.Fatal("missing job must remain nil")
	}
}

func TestVideoJobConcurrentReadSerialization(t *testing.T) {
	j := &videoJob{ID: "video_snapshot_concurrent", Status: "queued", CreatedAt: time.Now().Unix()}
	putVideoJob(j)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			videoJobsMu.Lock()
			j.Status = "in_progress"
			j.Account = "account"
			videoJobsMu.Unlock()
		}
	}()
	for i := 0; i < 100; i++ {
		if _, err := json.Marshal(getVideoJob(j.ID)); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}
