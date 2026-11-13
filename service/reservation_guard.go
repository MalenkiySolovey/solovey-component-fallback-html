//go:build !minimal

package service

import (
	"context"
	"errors"
	"fmt"
)

const reservationProviderID = "fallback-html"

func guardSiteTargetMutation(tx Store, siteID uint) error {
	return tx.GuardSiteMutation(siteID)
}

func (s *Service) guardedRuntimeMutation(siteID uint, mutate func(Store) error) error {
	ctx := s.db.Context()
	return s.db.WithMutationLockContext(ctx, func() error {
		activated := false
		err := s.db.Transaction(func(tx Store) error {
			if err := guardSiteTargetMutation(tx, siteID); err != nil {
				return err
			}
			if err := mutate(tx); err != nil {
				return err
			}
			activated = true
			return s.runtime.RebuildContext(ctx, tx)
		})
		if err == nil || !activated || errors.Is(err, ErrRuntimeDraining) {
			return err
		}
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runtimeDrainTimeout)
		defer cancel()
		if restoreErr := s.runtime.RebuildContext(recoveryCtx, s.db); restoreErr != nil {
			return errors.Join(err, fmt.Errorf("restore fallback-html runtime after rollback: %w", restoreErr))
		}
		return err
	})
}

func (s *Service) guardedSiteMutation(siteID uint, mutate func(Store) error, after func() error) error {
	return s.db.WithMutationLock(func() error {
		if err := s.db.Transaction(func(tx Store) error {
			if err := guardSiteTargetMutation(tx, siteID); err != nil {
				return err
			}
			return mutate(tx)
		}); err != nil {
			return err
		}
		if after != nil {
			return after()
		}
		return nil
	})
}
