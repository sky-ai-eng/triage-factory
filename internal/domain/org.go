package domain

import "time"

type Org struct {
	ID          string
	Name        string
	Slug        string
	OwnerUserID string
	IsPersonal  bool
	CreatedAt   time.Time
}
