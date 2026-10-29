package shadow

import (
	"context"
	"fmt"

	"github.com/jackc/pglogrepl"
)

// Status returns the lag figures without comparing data.
func Status(ctx context.Context, sourceURL, staskURL, slot string) (Lag, error) {
	src, err := connect(ctx, sourceURL, false)
	if err != nil {
		return Lag{}, err
	}
	defer src.Close(context.Background())
	st, err := connect(ctx, staskURL, false)
	if err != nil {
		return Lag{}, err
	}
	defer st.Close(context.Background())
	return measureLag(ctx, src, st, slot), nil
}

// Drop removes the slot and publication on the source and the control row in
// STASK (the shadowed tables stay).
func Drop(ctx context.Context, sourceURL, staskURL, slot, pub string) error {
	rc, err := connect(ctx, sourceURL, true)
	if err != nil {
		return err
	}
	defer rc.Close(context.Background())
	src, err := connect(ctx, sourceURL, false)
	if err != nil {
		return err
	}
	defer src.Close(context.Background())
	if r, err := query(ctx, src, "SELECT 1 FROM pg_replication_slots WHERE slot_name = $1", &slot); err == nil && len(r.Data) > 0 {
		if err := pglogrepl.DropReplicationSlot(ctx, rc, slot, pglogrepl.DropReplicationSlotOptions{Wait: true}); err != nil {
			return fmt.Errorf("drop slot: %w", err)
		}
	}
	if err := exec(ctx, src, "DROP PUBLICATION IF EXISTS "+qi(pub)); err != nil {
		return err
	}
	st, err := connect(ctx, staskURL, false)
	if err != nil {
		return err
	}
	defer st.Close(context.Background())
	if cols, ok, _ := stColumns(ctx, st, ctlTable); ok && len(cols) > 0 {
		_, err = query(ctx, st, "DELETE FROM "+ctlTable+" WHERE slot = $1", &slot)
	}
	return err
}
