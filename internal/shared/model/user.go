// Package model holds the domain types that cross package boundaries: the
// request context, the template envelope and the service layer all speak these
// rather than sqlc's generated structs.
//
// The reason is nullability. sqlc maps a nullable column to sql.NullString, and
// html/template renders that as "{jakarta true}" — the zero value of a struct,
// not the string anyone wanted. Unwrapping happens once, here, so no caller has
// to remember to do it.
package model

import (
	"github.com/remorac/appskep-tpj/internal/database/sqlc"
)

// User is the local mirror of an Appskep identity.
//
// Name and Email are owned by Appskep and refreshed from the JWT on every login;
// Phone, Address and AvatarPath are local and editable in Phase 9's profile page.
type User struct {
	ID int64
	// AppskepUserID is the `user_id` claim — the only identity key we trust.
	AppskepUserID uint64
	Name          string
	Email         string
	Phone         string
	Address       string
	AvatarPath    string
	Role          string
	IsActive      bool
}

// UserFromSQLC flattens the generated row. This is the only place the users
// table's nullable columns are unwrapped.
func UserFromSQLC(u sqlc.User) User {
	return User{
		ID:            u.ID,
		AppskepUserID: u.AppskepUserID,
		Name:          u.Name,
		Email:         u.Email,
		Phone:         u.Phone.String,
		Address:       u.Address.String,
		AvatarPath:    u.AvatarPath.String,
		Role:          string(u.Role),
		IsActive:      u.IsActive,
	}
}

// IsAdmin reports whether the user may reach the admin subsystem.
//
// The check is against the local users.role column, not against Appskep — see
// PLAN.md Q1. Role is re-read from the database on every request, so a demotion
// takes effect on the user's next click rather than at session expiry.
func (u User) IsAdmin() bool {
	return u.Role == string(sqlc.UsersRoleAdmin)
}
