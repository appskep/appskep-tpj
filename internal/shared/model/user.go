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
	"database/sql"
	"strconv"

	"github.com/remorac/appskep-tpj/internal/database/sqlc"
)

// birthdateLayout is the yyyy-mm-dd string form of users.birthdate, the same
// shape the HTML date input submits and the Appskep account API expects.
const birthdateLayout = "2006-01-02"

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
	// Birthdate is the yyyy-mm-dd string, empty when unset. Sex is "1", "2" or
	// empty — Appskep's convention. Both are mirrored from Appskep so the profile
	// form can prefill them; they are not in the JWT, so the mirror is their only
	// local copy.
	Birthdate string
	Sex       string
	Address   string
	// Latitude and Longitude are the saved map pin, empty when there is none.
	// Both or neither: the pair is written together and unwrapped together.
	Latitude   string
	Longitude  string
	AvatarPath string
	Role       string
	IsActive   bool
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
		Birthdate:     birthdateString(u.Birthdate),
		Sex:           sexString(u.Sex),
		Address:       u.Address.String,
		Latitude:      u.Latitude.String,
		Longitude:     u.Longitude.String,
		AvatarPath:    u.AvatarPath.String,
		Role:          string(u.Role),
		IsActive:      u.IsActive,
	}
}

// birthdateString renders users.birthdate (a nullable DATE) as yyyy-mm-dd, or
// "" when unset. The one place this column is unwrapped, per the package doc.
func birthdateString(t sql.NullTime) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format(birthdateLayout)
}

// sexString renders users.sex (a nullable TINYINT) as "1"/"2", or "" when unset.
func sexString(n sql.NullInt16) string {
	if !n.Valid {
		return ""
	}
	return strconv.FormatInt(int64(n.Int16), 10)
}

// IsAdmin reports whether the user may reach the admin subsystem.
//
// The check is against the local users.role column, not against Appskep — see
// PLAN.md Q1. Role is re-read from the database on every request, so a demotion
// takes effect on the user's next click rather than at session expiry.
func (u User) IsAdmin() bool {
	return u.Role == string(sqlc.UsersRoleAdmin)
}
