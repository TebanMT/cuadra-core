package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

type correctionFingerprintLine struct {
	SaleItemID string `json:"sale_item_id,omitempty"`
	ProductID  string `json:"product_id"`
	Quantity   int    `json:"quantity"`
}

// saleCorrectionFingerprint describes the complete semantic command. Line
// order is not meaningful, so it is canonicalized before hashing; every
// field that can change money, stock, authorization evidence or the drawer is
// included so a reused key cannot silently execute a different correction.
func saleCorrectionFingerprint(in CorrectSaleInput) (string, error) {
	lines := make([]correctionFingerprintLine, 0, len(in.Lines))
	for _, line := range in.Lines {
		row := correctionFingerprintLine{ProductID: line.ProductID.String(), Quantity: line.Quantity}
		if line.SaleItemID != nil {
			row.SaleItemID = line.SaleItemID.String()
		}
		lines = append(lines, row)
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].SaleItemID != lines[j].SaleItemID {
			return lines[i].SaleItemID < lines[j].SaleItemID
		}
		if lines[i].ProductID != lines[j].ProductID {
			return lines[i].ProductID < lines[j].ProductID
		}
		return lines[i].Quantity < lines[j].Quantity
	})

	drawerID := ""
	if in.RefundCashDrawerID != nil {
		drawerID = in.RefundCashDrawerID.String()
	}
	collectionDrawerID := ""
	if in.CollectionCashDrawerID != nil {
		collectionDrawerID = in.CollectionCashDrawerID.String()
	}
	payload := struct {
		GymID                  string                      `json:"gym_id"`
		ActorUserID            string                      `json:"actor_user_id"`
		ActorRole              string                      `json:"actor_role"`
		SaleID                 string                      `json:"sale_id"`
		ExpectedVersion        int                         `json:"expected_version"`
		Annul                  bool                        `json:"annul"`
		Reason                 string                      `json:"reason"`
		MoneyResolution        string                      `json:"money_resolution"`
		IncreaseResolution     string                      `json:"increase_resolution"`
		RefundMethod           string                      `json:"refund_method"`
		CashDrawerID           string                      `json:"cash_drawer_id"`
		CollectionMethod       string                      `json:"collection_method"`
		CollectionCashDrawerID string                      `json:"collection_cash_drawer_id"`
		CollectionDate         string                      `json:"collection_date"`
		Lines                  []correctionFingerprintLine `json:"lines"`
	}{
		GymID: in.GymID.String(), ActorUserID: in.ActorUserID.String(), ActorRole: strings.TrimSpace(in.ActorRole),
		SaleID: in.SaleID.String(), ExpectedVersion: in.ExpectedVersion, Annul: in.Annul, Reason: strings.TrimSpace(in.Reason),
		MoneyResolution: strings.TrimSpace(in.MoneyResolution), IncreaseResolution: strings.TrimSpace(in.IncreaseResolution),
		RefundMethod: strings.TrimSpace(in.RefundMethod), CashDrawerID: drawerID,
		CollectionMethod: strings.TrimSpace(in.CollectionMethod), CollectionCashDrawerID: collectionDrawerID,
		CollectionDate: correctionFingerprintDate(in.CollectionDate), Lines: lines,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func correctionFingerprintDate(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02")
}
