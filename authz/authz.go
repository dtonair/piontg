package authz

// Authorizer enforces the single-user Telegram security model.
type Authorizer struct {
	allowedUserID int64
}

func New(allowedUserID int64) Authorizer {
	return Authorizer{allowedUserID: allowedUserID}
}

func (a Authorizer) AllowedUserID() int64 { return a.allowedUserID }

func (a Authorizer) IsAllowed(userID int64) bool {
	return a.allowedUserID > 0 && userID == a.allowedUserID
}
