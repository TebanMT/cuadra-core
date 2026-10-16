//go:build server

package repositories

import (
	"errors"
	n "github.com/cuadra/cuadra-core/src/modules/notifications/domain/notification"
	"github.com/cuadra/cuadra-core/src/modules/notifications/infraestructure/db/models"
	shared "github.com/cuadra/cuadra-core/src/shared/domain"
	"github.com/cuadra/cuadra-core/src/shared/phone"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strconv"
	"strings"
	"time"
)

func (r *NotificationPostgresRepository) LockHeld(tx shared.Transaction, gym, id uuid.UUID) (*n.Notification, error) {
	var m models.NotificationModel
	err := tx.(*shared.GormTransaction).Tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("gym_id=? AND id=?", gym, id).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return notificationFromModel(&m)
}
func (r *NotificationPostgresRepository) OldHeld(tx shared.Transaction, before time.Time, limit int) ([]*n.Notification, error) {
	var rows []models.NotificationModel
	err := tx.(*shared.GormTransaction).Tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).Where("status='held' AND deleted_at IS NULL AND updated_at<?", before).Order("updated_at,id").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	out := make([]*n.Notification, 0, len(rows))
	for _, row := range rows {
		v, err := notificationFromModel(&row)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (r *NotificationPostgresRepository) RecoveryRecipient(tx shared.Transaction, msg *n.Notification) (string, error) {
	g := tx.(*shared.GormTransaction).Tx
	var contact struct {
		Phone, Email string
		MemberNumber int
	}
	var result *gorm.DB
	if msg.RecipientType == n.RecipientMember {
		result = g.Raw(`SELECT phone,COALESCE(email,'') AS email,COALESCE(member_number,0) AS member_number FROM members WHERE gym_id=? AND id=? AND deleted_at IS NULL AND status='active'`, msg.GymID, msg.RecipientID).Scan(&contact)
	} else {
		result = g.Raw(`SELECT COALESCE(phone,'') AS phone,email FROM users WHERE gym_id=? AND id=? AND deleted_at IS NULL AND active=true`, msg.GymID, msg.RecipientID).Scan(&contact)
	}
	if result.Error != nil {
		return "", result.Error
	}
	if result.RowsAffected == 0 {
		return "El destinatario ya no está activo.", nil
	}
	if (msg.Channel == n.ChannelWhatsApp && (phone.Normalize(contact.Phone) != phone.Normalize(msg.RecipientAddress) || !phone.Valid(phone.Normalize(contact.Phone)))) || (msg.Channel == n.ChannelEmail && (contact.Email == "" || !strings.EqualFold(contact.Email, msg.RecipientAddress))) {
		return "El contacto cambió. Genera un mensaje nuevo con sus datos actuales.", nil
	}
	if msg.TemplateKey == "member_welcome_number" && (contact.MemberNumber <= 0 || strconv.Itoa(contact.MemberNumber) != msg.Payload["member_number"]) {
		return "El número de socio cambió. Genera una bienvenida nueva.", nil
	}
	if strings.HasPrefix(msg.TemplateKey, "receipt_") {
		id, err := uuid.Parse(msg.Payload["payment_id"])
		if err != nil {
			return "No se encontró el pago del comprobante.", nil
		}
		var count int64
		if err = g.Table("payments").Where("gym_id=? AND id=? AND member_id=? AND deleted_at IS NULL AND amount>0", msg.GymID, id, msg.RecipientID).Count(&count).Error; err != nil {
			return "", err
		}
		if count == 0 {
			return "El pago ya no está disponible. Revisa el comprobante desde Pagos.", nil
		}
	}
	return "", nil
}
