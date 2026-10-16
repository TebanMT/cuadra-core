//go:build server

package sync

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Payment UUIDs identify operations; a locally allocated display folio cannot
// reject a second legitimate offline payment. Serialize with the native cloud
// allocator, preserve an accepted folio on retries, and record the replacement
// in the journal so the push acknowledgment can rebase the source immediately.
func projectPaymentWithFolio(g *gorm.DB, table EntityTable, gym, id uuid.UUID, payload []byte) error {
	raw, err := decodeProjectorPayload("payments", payload)
	if err != nil {
		return err
	}
	incoming, _ := raw["folio"].(string)
	if err = g.Exec(`SELECT pg_advisory_xact_lock(hashtextextended(?,0))`, "tinta:payment-folio:"+gym.String()).Error; err != nil {
		return err
	}
	var canonical string
	if err = g.Raw(`SELECT folio FROM payments WHERE gym_id=? AND id=?`, gym, id).Scan(&canonical).Error; err != nil {
		return err
	}
	if canonical == "" {
		canonical = incoming
		var occupied bool
		if err = g.Raw(`SELECT EXISTS(SELECT 1 FROM payments WHERE gym_id=? AND folio=? AND id<>?)`, gym, incoming, id).Scan(&occupied).Error; err != nil {
			return err
		}
		if occupied {
			prefix := strings.SplitN(incoming, "-", 2)[0]
			var last int64
			if err = g.Raw(`SELECT COALESCE(MAX(split_part(folio,'-',2)::bigint),0) FROM payments WHERE gym_id=? AND split_part(folio,'-',1)=? AND split_part(folio,'-',2) ~ '^[0-9]+$'`, gym, prefix).Scan(&last).Error; err != nil {
				return err
			}
			canonical = fmt.Sprintf("%s-%06d", prefix, last+1)
		}
	}
	if canonical != incoming {
		raw["folio"] = canonical
		payload, err = json.Marshal(raw)
		if err != nil {
			return err
		}
	}
	if err = projectPayment(g, table, gym, id, payload); err != nil {
		return err
	}
	if canonical != incoming {
		if err := g.Exec(`UPDATE sync_entities SET payload=payload || jsonb_build_object('folio',?::text,'version',version+1),version=version+1 WHERE gym_id=? AND entity_type='payments' AND entity_id=?`, canonical, gym, id).Error; err != nil {
			return err
		}
		// A later native correction must advance beyond the acknowledgment
		// already stored by desktops, including the originating terminal.
		return g.Exec(`UPDATE payments p SET version=GREATEST(p.version,s.version) FROM sync_entities s WHERE p.gym_id=? AND p.id=? AND s.gym_id=p.gym_id AND s.entity_type='payments' AND s.entity_id=p.id`, gym, id).Error
	}
	return nil
}
