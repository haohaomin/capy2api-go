package server

import (
	"testing"
	"time"
)

func TestNoteFailureUnbindsAfterTwo(t *testing.T) {
	r := newConvRegistry(time.Minute, 8, "")
	r.bind("k", "jam_1", "acc", "org", "model", "sys")
	if dropped := r.noteFailure("k", false); dropped {
		t.Fatal("第一次超时不应解绑")
	}
	if dropped := r.noteFailure("k", false); !dropped {
		t.Fatal("连续第二次超时应解绑")
	}
	r.mu.Lock()
	_, exists := r.by["k"]
	r.mu.Unlock()
	if exists {
		t.Fatal("解绑后条目应被删除")
	}
}

func TestNoteFailureHardDropsImmediately(t *testing.T) {
	r := newConvRegistry(time.Minute, 8, "")
	r.bind("k2", "jam_2", "acc", "org", "model", "sys")
	if dropped := r.noteFailure("k2", true); !dropped {
		t.Fatal("hard 失败应立即解绑")
	}
}
