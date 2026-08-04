package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

func TestStoreAttachmentBlobConcurrentDifferentContentHonorsQuota(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "quota-race")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE org_attachment_usage SET bytes_quota = 2 WHERE org_id = $1`, orgID); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		errorsByUpload := make(chan error, 2)
		var wait sync.WaitGroup
		for _, content := range [][]byte{{1, 2}, {3, 4}} {
			content := content
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, _, storeErr := st.StoreAttachmentBlob(ctx, orgID, "application/octet-stream", content)
				errorsByUpload <- storeErr
			}()
		}
		close(start)
		wait.Wait()
		close(errorsByUpload)

		var succeeded, quotaRejected int
		for uploadErr := range errorsByUpload {
			switch {
			case uploadErr == nil:
				succeeded++
			case errors.Is(uploadErr, ErrAttachmentQuotaExceeded):
				quotaRejected++
			default:
				t.Fatalf("unexpected upload error: %v", uploadErr)
			}
		}
		if succeeded != 1 || quotaRejected != 1 {
			t.Fatalf("succeeded=%d quotaRejected=%d, want 1/1", succeeded, quotaRejected)
		}
		assertAttachmentUsage(t, ctx, db, orgID, 2, 1)
	})
}

func TestStoreAttachmentBlobConcurrentUploadsCannotExceedQuota(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "quota-race-twenty")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE org_attachment_usage SET bytes_quota = 10 WHERE org_id = $1`, orgID); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		errorsByUpload := make(chan error, 20)
		var wait sync.WaitGroup
		for value := byte(1); value <= 20; value++ {
			value := value
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, _, storeErr := st.StoreAttachmentBlob(ctx, orgID, "application/octet-stream", []byte{value})
				errorsByUpload <- storeErr
			}()
		}
		close(start)
		wait.Wait()
		close(errorsByUpload)

		var succeeded, quotaRejected int
		for uploadErr := range errorsByUpload {
			switch {
			case uploadErr == nil:
				succeeded++
			case errors.Is(uploadErr, ErrAttachmentQuotaExceeded):
				quotaRejected++
			default:
				t.Fatalf("unexpected upload error: %v", uploadErr)
			}
		}
		if succeeded != 10 || quotaRejected != 10 {
			t.Fatalf("succeeded=%d quotaRejected=%d, want 10/10", succeeded, quotaRejected)
		}
		assertAttachmentUsage(t, ctx, db, orgID, 10, 10)
	})
}

func TestStoreAttachmentBlobConcurrentSameContentChargesOnce(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "dedup-race")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE org_attachment_usage SET bytes_quota = 2 WHERE org_id = $1`, orgID); err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		insertedResults := make(chan bool, 2)
		errorsByUpload := make(chan error, 2)
		var wait sync.WaitGroup
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, inserted, storeErr := st.StoreAttachmentBlob(ctx, orgID, "text/plain", []byte("ok"))
				insertedResults <- inserted
				errorsByUpload <- storeErr
			}()
		}
		close(start)
		wait.Wait()
		close(insertedResults)
		close(errorsByUpload)

		for uploadErr := range errorsByUpload {
			if uploadErr != nil {
				t.Fatal(uploadErr)
			}
		}
		var insertedCount int
		for inserted := range insertedResults {
			if inserted {
				insertedCount++
			}
		}
		if insertedCount != 1 {
			t.Fatalf("inserted count=%d, want 1", insertedCount)
		}
		assertAttachmentUsage(t, ctx, db, orgID, 2, 1)
	})
}

func TestStoreAttachmentBlobQuotaFailureRollsBackAndCannotBypassOnRetry(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "quota-rollback")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE org_attachment_usage SET bytes_quota = 1 WHERE org_id = $1`, orgID); err != nil {
			t.Fatal(err)
		}

		for attempt := 1; attempt <= 2; attempt++ {
			_, inserted, storeErr := st.StoreAttachmentBlob(ctx, orgID, "text/plain", []byte("too large"))
			if !errors.Is(storeErr, ErrAttachmentQuotaExceeded) || inserted {
				t.Fatalf("attempt %d inserted=%v err=%v", attempt, inserted, storeErr)
			}
			assertAttachmentUsage(t, ctx, db, orgID, 0, 0)
		}
	})
}

func TestStoreAttachmentBlobRepairsMissingUsageRow(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "missing-usage-row")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM org_attachment_usage WHERE org_id = $1`, orgID); err != nil {
			t.Fatal(err)
		}

		if _, inserted, err := st.StoreAttachmentBlob(ctx, orgID, "text/plain", []byte("repaired")); err != nil || !inserted {
			t.Fatalf("inserted=%v err=%v", inserted, err)
		}
		assertAttachmentUsage(t, ctx, db, orgID, int64(len("repaired")), 1)
	})
}

func TestStoreAttachmentBlobRestoresExistingBytesWhenUsageRowIsMissing(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "missing-usage-with-blobs")
		if err != nil {
			t.Fatal(err)
		}
		if _, inserted, err := st.StoreAttachmentBlob(ctx, orgID, "text/plain", []byte("first")); err != nil || !inserted {
			t.Fatalf("first insert=%v err=%v", inserted, err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM org_attachment_usage WHERE org_id = $1`, orgID); err != nil {
			t.Fatal(err)
		}

		if _, inserted, err := st.StoreAttachmentBlob(ctx, orgID, "text/plain", []byte("second")); err != nil || !inserted {
			t.Fatalf("second insert=%v err=%v", inserted, err)
		}
		assertAttachmentUsage(t, ctx, db, orgID, int64(len("first")+len("second")), 2)
	})
}

func TestStoreAttachmentBlobExistingContentAllowedAboveQuotaAndLoads(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "existing-over-quota")
		if err != nil {
			t.Fatal(err)
		}
		content := []byte("stored")
		digest, inserted, err := st.StoreAttachmentBlob(ctx, orgID, "text/plain", content)
		if err != nil || !inserted {
			t.Fatalf("initial insert=%v err=%v", inserted, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE org_attachment_usage SET bytes_quota = 0 WHERE org_id = $1`, orgID); err != nil {
			t.Fatal(err)
		}
		replayedDigest, inserted, err := st.StoreAttachmentBlob(ctx, orgID, "text/plain", content)
		if err != nil || inserted || replayedDigest != digest {
			t.Fatalf("replay digest=%q inserted=%v err=%v", replayedDigest, inserted, err)
		}

		info, err := st.GetAttachmentBlobInfo(ctx, orgID, digest)
		if err != nil || info.SizeBytes != int64(len(content)) || info.ContentType != "text/plain" || info.RefCount != 0 {
			t.Fatalf("blob info=%+v err=%v", info, err)
		}
		loaded, err := st.LoadAttachmentBlob(ctx, orgID, digest)
		if err != nil || !bytes.Equal(loaded, content) {
			t.Fatalf("loaded=%q err=%v", loaded, err)
		}
	})
}

func TestStoreAttachmentBlobRollsBackWithRunAsOrg(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "blob-outer-rollback")
		if err != nil {
			t.Fatal(err)
		}
		sentinel := errors.New("force outer rollback")
		err = st.RunAsOrg(ctx, orgID, func(scoped *Store) error {
			if _, inserted, err := scoped.StoreAttachmentBlob(ctx, orgID, "text/plain", []byte("rollback")); err != nil || !inserted {
				t.Fatalf("store in outer transaction inserted=%v err=%v", inserted, err)
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("RunAsOrg err=%v, want sentinel", err)
		}
		assertAttachmentUsage(t, ctx, db, orgID, 0, 0)
	})
}

func assertAttachmentUsage(t *testing.T, ctx context.Context, db *sql.DB, orgID string, wantUsed int64, wantBlobs int) {
	t.Helper()
	var used int64
	if err := db.QueryRowContext(ctx, `SELECT bytes_used FROM org_attachment_usage WHERE org_id = $1`, orgID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != wantUsed {
		t.Fatalf("bytes_used=%d, want %d", used, wantUsed)
	}
	var blobs int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM attachment_blobs WHERE org_id = $1`, orgID).Scan(&blobs); err != nil {
		t.Fatal(err)
	}
	if blobs != wantBlobs {
		t.Fatalf("blob count=%d, want %d", blobs, wantBlobs)
	}
}
