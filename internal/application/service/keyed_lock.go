package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/common/redislock"
	"github.com/redis/go-redis/v9"
)

const (
	serviceLockLease         = 30 * time.Second
	serviceLockRenewInterval = 10 * time.Second
)

type localKeyedLocks struct {
	mu    sync.Mutex
	locks map[string]*localKeyedLock
}

type localKeyedLock struct {
	mu   sync.Mutex
	refs int
}

var serviceLocalLocks = localKeyedLocks{
	locks: make(map[string]*localKeyedLock),
}

func withServiceLock(
	ctx context.Context,
	redisClient *redis.Client,
	key string,
	fn func(context.Context) error,
) error {
	if redisClient != nil {
		return redislock.WithRenewableLock(
			ctx,
			redisClient,
			key,
			serviceLockLease,
			serviceLockRenewInterval,
			fn,
		)
	}

	unlock := serviceLocalLocks.lock(key)
	defer unlock()
	return fn(ctx)
}

func (l *localKeyedLocks) lock(key string) func() {
	l.mu.Lock()
	entry := l.locks[key]
	if entry == nil {
		entry = &localKeyedLock{}
		l.locks[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}

func knowledgeProcessingLockKey(tenantID uint64, knowledgeID string) string {
	return fmt.Sprintf("weknora:knowledge-processing:%d:%s", tenantID, knowledgeID)
}

func (s *knowledgeService) withKnowledgeProcessingLock(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	fn func(context.Context) error,
) error {
	return withServiceLock(ctx, s.redisClient, knowledgeProcessingLockKey(tenantID, knowledgeID), fn)
}
