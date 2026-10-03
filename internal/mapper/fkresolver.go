package mapper

import (
	"context"
	"fmt"

	"github.com/lfsoft/sync/internal/idmap"
)

type IDMapFKResolver struct {
	repo *idmap.Repository
}

func NewIDMapFKResolver(repo *idmap.Repository) *IDMapFKResolver {
	return &IDMapFKResolver{repo: repo}
}

func (r *IDMapFKResolver) ResolveLegacyToPG(ctx context.Context, entity, legacyID string) (int64, error) {
	entry, err := r.repo.GetByLegacy(ctx, entity, legacyID)
	if err != nil {
		return 0, fmt.Errorf("id_map lookup %s legacy=%s: %w", entity, legacyID, err)
	}
	return entry.PGID, nil
}

func (r *IDMapFKResolver) ResolvePGToLegacy(ctx context.Context, entity string, pgID int64) (string, error) {
	entry, err := r.repo.GetByPGID(ctx, entity, pgID)
	if err != nil {
		return "", fmt.Errorf("id_map lookup %s pgId=%d: %w", entity, pgID, err)
	}
	return entry.LegacyID, nil
}
