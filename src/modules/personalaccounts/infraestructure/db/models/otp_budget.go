//go:build server

package models

import "time"

type OTPBudgetModel struct {
	Scope        string     `gorm:"column:scope;primaryKey"`
	SubjectHash  []byte     `gorm:"column:subject_hash;primaryKey"`
	WindowStart  time.Time  `gorm:"column:window_start;primaryKey"`
	SendCount    int        `gorm:"column:send_count"`
	CheckCount   int        `gorm:"column:check_count"`
	BlockedUntil *time.Time `gorm:"column:blocked_until"`
}

func (OTPBudgetModel) TableName() string { return "mancuerna.otp_budgets" }
