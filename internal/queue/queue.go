package queue

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type Queue struct {
	client *redis.Client
}

func New(url string) (*Queue, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	client := redis.NewClient(opt)
	return &Queue{client: client}, nil
}

func (q *Queue) Ping(ctx context.Context) error {
	return q.client.Ping(ctx).Err()
}

func (q *Queue) PushEmbeddingJob(ctx context.Context, messageID string) error {
	return q.client.LPush(ctx, "embedding_jobs", messageID).Err()
}

func (q *Queue) PopEmbeddingJob(ctx context.Context, timeout time.Duration) (string, error) {
	res, err := q.client.BRPop(ctx, timeout, "embedding_jobs").Result()
	if err != nil {
		return "", err
	}
	if len(res) < 2 {
		return "", redis.Nil
	}
	return res[1], nil
}

func (q *Queue) Depth(ctx context.Context) (int64, error) {
	return q.client.LLen(ctx, "embedding_jobs").Result()
}

func (q *Queue) Close() error {
	return q.client.Close()
}
