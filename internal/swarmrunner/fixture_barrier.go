package swarmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spexus-ai/spexus-agent/internal/swarm"
)

// LocalFixtureClaimBarrier is deliberately absent from Config's JSON wire.
// Only the local runner command may enable it for an isolated fixture run.
func (r *Runner) EnableLocalFixtureClaimBarrier() error {
	state, err := os.Lstat(r.cfg.StateDirectory)
	if err != nil {
		return err
	}
	if err := ownedFixtureDirectory(state, false); err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	dir := filepath.Join(r.cfg.StateDirectory, "claim-barrier")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := ownedFixtureDirectory(info, true); err != nil {
		return fmt.Errorf("claim barrier directory: %w", err)
	}
	r.fixtureClaimBarrierDir = dir
	return nil
}

func ownedFixtureDirectory(info os.FileInfo, private bool) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 || private && info.Mode().Perm()&0077 != 0 {
		return errors.New("must be an owned directory without symlinks or shared write access; barrier directory must be private")
	}
	return nil
}

func (r *Runner) waitForLocalFixtureClaimRelease(ctx context.Context, d swarm.Delivery, p profile, claim swarm.LaunchClaim) error {
	if r.fixtureClaimBarrierDir == "" {
		return nil
	}
	base := filepath.Join(r.fixtureClaimBarrierDir, "claim-"+claim.ClaimID)
	ready := base + ".ready"
	release := base + ".release"
	marker, err := json.Marshal(struct {
		ClaimID      string             `json:"claim_id"`
		ProfileID    string             `json:"profile_id"`
		Revision     string             `json:"revision"`
		ExecutionRef swarm.ExecutionRef `json:"execution_ref"`
		MailboxSeq   int64              `json:"mailbox_seq"`
	}{claim.ClaimID, p.ID, p.Revision, claim.ExecutionRef, d.MailboxSeq})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(ready, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(marker, '\n')); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := os.Lstat(release)
		if err == nil {
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
				return errors.New("claim release must be an owned private regular file")
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
