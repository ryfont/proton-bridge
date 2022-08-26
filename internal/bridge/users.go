package bridge

import (
	"context"
	"fmt"

	"github.com/ProtonMail/gluon/imap"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
	"github.com/ProtonMail/proton-bridge/v2/internal/events"
	"github.com/ProtonMail/proton-bridge/v2/internal/user"
	"github.com/ProtonMail/proton-bridge/v2/internal/vault"
	"github.com/go-resty/resty/v2"
	"github.com/sirupsen/logrus"
	"gitlab.protontech.ch/go/liteapi"
	"golang.org/x/exp/slices"
)

type UserInfo struct {
	// UserID is the user's API ID.
	UserID string

	// Username is the user's API username.
	Username string

	// Connected is true if the user is logged in (has API auth).
	Connected bool

	// Addresses holds the user's email addresses. The first address is the primary address.
	Addresses []string

	// AddressMode is the user's address mode.
	AddressMode AddressMode

	// BridgePass is the user's bridge password.
	BridgePass string

	// UsedSpace is the amount of space used by the user.
	UsedSpace int

	// MaxSpace is the total amount of space available to the user.
	MaxSpace int
}

type AddressMode int

const (
	SplitMode AddressMode = iota
	CombinedMode
)

// GetUserIDs returns the IDs of all known users (authorized or not).
func (bridge *Bridge) GetUserIDs() []string {
	return bridge.vault.GetUserIDs()
}

// GetUserInfo returns info about the given user.
func (bridge *Bridge) GetUserInfo(userID string) (UserInfo, error) {
	vaultUser, err := bridge.vault.GetUser(userID)
	if err != nil {
		return UserInfo{}, err
	}

	user, ok := bridge.users[userID]
	if !ok {
		return getUserInfo(vaultUser.UserID(), vaultUser.Username()), nil
	}

	return getConnUserInfo(user), nil
}

// QueryUserInfo queries the user info by username or address.
func (bridge *Bridge) QueryUserInfo(query string) (UserInfo, error) {
	for userID, user := range bridge.users {
		if user.Match(query) {
			return bridge.GetUserInfo(userID)
		}
	}

	return UserInfo{}, ErrNoSuchUser
}

// LoginUser authorizes a new bridge user with the given username and password.
// If necessary, a TOTP and mailbox password are requested via the callbacks.
func (bridge *Bridge) LoginUser(
	ctx context.Context,
	username, password string,
	getTOTP func() (string, error),
	getKeyPass func() ([]byte, error),
) (string, error) {
	client, auth, err := bridge.api.NewClientWithLogin(ctx, username, password)
	if err != nil {
		return "", err
	}

	if auth.TwoFA.Enabled == liteapi.TOTPEnabled {
		totp, err := getTOTP()
		if err != nil {
			return "", err
		}

		if err := client.Auth2FA(ctx, liteapi.Auth2FAReq{TwoFactorCode: totp}); err != nil {
			return "", err
		}
	}

	var keyPass []byte

	if auth.PasswordMode == liteapi.TwoPasswordMode {
		pass, err := getKeyPass()
		if err != nil {
			return "", err
		}

		keyPass = pass
	} else {
		keyPass = []byte(password)
	}

	apiUser, apiAddrs, userKR, addrKRs, saltedKeyPass, err := client.Unlock(ctx, keyPass)
	if err != nil {
		return "", err
	}

	if err := bridge.addUser(ctx, client, apiUser, apiAddrs, userKR, addrKRs, auth.UID, auth.RefreshToken, saltedKeyPass); err != nil {
		return "", err
	}

	return apiUser.ID, nil
}

// LogoutUser logs out the given user.
func (bridge *Bridge) LogoutUser(ctx context.Context, userID string) error {
	return bridge.logoutUser(ctx, userID, true, false)
}

// DeleteUser deletes the given user.
// If it is authorized, it is logged out first.
func (bridge *Bridge) DeleteUser(ctx context.Context, userID string) error {
	if bridge.users[userID] != nil {
		if err := bridge.logoutUser(ctx, userID, true, true); err != nil {
			return err
		}
	}

	if err := bridge.vault.DeleteUser(userID); err != nil {
		return err
	}

	bridge.publish(events.UserDeleted{
		UserID: userID,
	})

	return nil
}

func (bridge *Bridge) GetAddressMode(userID string) (AddressMode, error) {
	panic("TODO")
}

func (bridge *Bridge) SetAddressMode(userID string, mode AddressMode) error {
	panic("TODO")
}

// loadUsers loads authorized users from the vault.
func (bridge *Bridge) loadUsers(ctx context.Context) error {
	for _, userID := range bridge.vault.GetUserIDs() {
		user, err := bridge.vault.GetUser(userID)
		if err != nil {
			return err
		}

		if user.AuthUID() == "" {
			continue
		}

		if err := bridge.loadUser(ctx, user); err != nil {
			logrus.WithError(err).Error("Failed to load connected user")

			if err := user.Clear(); err != nil {
				logrus.WithError(err).Error("Failed to clear user")
			}

			continue
		}
	}

	return nil
}

func (bridge *Bridge) loadUser(ctx context.Context, user *vault.User) error {
	client, auth, err := bridge.api.NewClientWithRefresh(ctx, user.AuthUID(), user.AuthRef())
	if err != nil {
		return fmt.Errorf("failed to create API client: %w", err)
	}

	apiUser, apiAddrs, userKR, addrKRs, err := client.UnlockSalted(ctx, user.KeyPass())
	if err != nil {
		return fmt.Errorf("failed to unlock user: %w", err)
	}

	if err := bridge.addUser(ctx, client, apiUser, apiAddrs, userKR, addrKRs, auth.UID, auth.RefreshToken, user.KeyPass()); err != nil {
		return fmt.Errorf("failed to add user: %w", err)
	}

	bridge.publish(events.UserLoggedIn{
		UserID: user.UserID(),
	})

	return nil
}

// addUser adds a new user with an already salted mailbox password.
func (bridge *Bridge) addUser(
	ctx context.Context,
	client *liteapi.Client,
	apiUser liteapi.User,
	apiAddrs []liteapi.Address,
	userKR *crypto.KeyRing,
	addrKRs map[string]*crypto.KeyRing,
	authUID, authRef string,
	saltedKeyPass []byte,
) error {
	if _, ok := bridge.users[apiUser.ID]; ok {
		return ErrUserAlreadyLoggedIn
	}

	var user *user.User

	if slices.Contains(bridge.vault.GetUserIDs(), apiUser.ID) {
		existingUser, err := bridge.addExistingUser(ctx, client, apiUser, apiAddrs, userKR, addrKRs, authUID, authRef, saltedKeyPass)
		if err != nil {
			return err
		}

		user = existingUser
	} else {
		newUser, err := bridge.addNewUser(ctx, client, apiUser, apiAddrs, userKR, addrKRs, authUID, authRef, saltedKeyPass)
		if err != nil {
			return err
		}

		user = newUser
	}

	go func() {
		for event := range user.GetNotifyCh() {
			switch event := event.(type) {
			case events.UserDeauth:
				if err := bridge.logoutUser(context.Background(), event.UserID, false, false); err != nil {
					logrus.WithError(err).Error("Failed to logout user")
				}
			}

			bridge.publish(event)
		}
	}()

	// Gluon will set the IMAP ID in the context, if known, before making requests on behalf of this user.
	client.AddPreRequestHook(func(ctx context.Context, req *resty.Request) error {
		if imapID, ok := imap.GetIMAPIDFromContext(ctx); ok {
			bridge.identifier.SetClient(imapID.Name, imapID.Version)
		}

		return nil
	})

	bridge.publish(events.UserLoggedIn{
		UserID: user.ID(),
	})

	return nil
}

func (bridge *Bridge) addNewUser(
	ctx context.Context,
	client *liteapi.Client,
	apiUser liteapi.User,
	apiAddrs []liteapi.Address,
	userKR *crypto.KeyRing,
	addrKRs map[string]*crypto.KeyRing,
	authUID, authRef string,
	saltedKeyPass []byte,
) (*user.User, error) {
	vaultUser, err := bridge.vault.AddUser(apiUser.ID, apiUser.Name, authUID, authRef, saltedKeyPass)
	if err != nil {
		return nil, err
	}

	user, err := user.New(ctx, vaultUser, client, apiUser, apiAddrs, userKR, addrKRs)
	if err != nil {
		return nil, err
	}

	gluonKey, err := crypto.RandomToken(32)
	if err != nil {
		return nil, err
	}

	imapConn, err := user.NewGluonConnector(ctx)
	if err != nil {
		return nil, err
	}

	gluonID, err := bridge.imapServer.AddUser(ctx, imapConn, gluonKey)
	if err != nil {
		return nil, err
	}

	if err := vaultUser.UpdateGluonData(gluonID, gluonKey); err != nil {
		return nil, err
	}

	if err := bridge.smtpBackend.addUser(user); err != nil {
		return nil, err
	}

	bridge.users[apiUser.ID] = user

	return user, nil
}

func (bridge *Bridge) addExistingUser(
	ctx context.Context,
	client *liteapi.Client,
	apiUser liteapi.User,
	apiAddrs []liteapi.Address,
	userKR *crypto.KeyRing,
	addrKRs map[string]*crypto.KeyRing,
	authUID, authRef string,
	saltedKeyPass []byte,
) (*user.User, error) {
	vaultUser, err := bridge.vault.GetUser(apiUser.ID)
	if err != nil {
		return nil, err
	}

	if err := vaultUser.UpdateAuth(authUID, authRef); err != nil {
		return nil, err
	}

	if err := vaultUser.UpdateKeyPass(saltedKeyPass); err != nil {
		return nil, err
	}

	user, err := user.New(ctx, vaultUser, client, apiUser, apiAddrs, userKR, addrKRs)
	if err != nil {
		return nil, err
	}

	imapConn, err := user.NewGluonConnector(ctx)
	if err != nil {
		return nil, err
	}

	if err := bridge.imapServer.LoadUser(ctx, imapConn, user.GluonID(), user.GluonKey()); err != nil {
		return nil, err
	}

	if err := bridge.smtpBackend.addUser(user); err != nil {
		return nil, err
	}

	bridge.users[apiUser.ID] = user

	return user, nil
}

// logoutUser closes and removes the user with the given ID.
// If withAPI is true, the user will additionally be logged out from API.
// If withFiles is true, the user's files will be deleted.
func (bridge *Bridge) logoutUser(ctx context.Context, userID string, withAPI, withFiles bool) error {
	user, ok := bridge.users[userID]
	if !ok {
		return ErrNoSuchUser
	}

	vaultUser, err := bridge.vault.GetUser(userID)
	if err != nil {
		return err
	}

	if err := bridge.imapServer.RemoveUser(ctx, vaultUser.GluonID(), withFiles); err != nil {
		return err
	}

	if err := bridge.smtpBackend.removeUser(user); err != nil {
		return err
	}

	if withAPI {
		if err := user.Logout(ctx); err != nil {
			return err
		}
	}

	if err := user.Close(ctx); err != nil {
		return err
	}

	if err := vaultUser.Clear(); err != nil {
		return err
	}

	delete(bridge.users, userID)

	bridge.publish(events.UserLoggedOut{
		UserID: userID,
	})

	return nil
}

// getUserInfo returns information about a disconnected user.
func getUserInfo(userID, username string) UserInfo {
	return UserInfo{
		UserID:      userID,
		Username:    username,
		AddressMode: CombinedMode,
	}
}

// getConnUserInfo returns information about a connected user.
func getConnUserInfo(user *user.User) UserInfo {
	return UserInfo{
		Connected:   true,
		UserID:      user.ID(),
		Username:    user.Name(),
		Addresses:   user.Addresses(),
		AddressMode: CombinedMode,
		BridgePass:  user.BridgePass(),
		UsedSpace:   user.UsedSpace(),
		MaxSpace:    user.MaxSpace(),
	}
}