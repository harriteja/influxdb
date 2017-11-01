package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/influxdata/chronograf"
	"github.com/influxdata/chronograf/oauth2"
)

// AuthorizedToken extracts the token and validates; if valid the next handler
// will be run.  The principal will be sent to the next handler via the request's
// Context.  It is up to the next handler to determine if the principal has access.
// On failure, will return http.StatusForbidden.
func AuthorizedToken(auth oauth2.Authenticator, logger chronograf.Logger, next http.Handler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log := logger.
			WithField("component", "token_auth").
			WithField("remote_addr", r.RemoteAddr).
			WithField("method", r.Method).
			WithField("url", r.URL)

		ctx := r.Context()
		// We do not check the authorization of the principal.  Those
		// served further down the chain should do so.
		principal, err := auth.Validate(ctx, r)
		if err != nil {
			log.Error("Invalid principal")
			w.WriteHeader(http.StatusForbidden)
			return
		}

		// If the principal is valid we will extend its lifespan
		// into the future
		principal, err = auth.Extend(ctx, w, principal)
		if err != nil {
			log.Error("Unable to extend principal")
			w.WriteHeader(http.StatusForbidden)
			return
		}

		// Send the principal to the next handler
		ctx = context.WithValue(ctx, oauth2.PrincipalKey, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
		return
	})
}

// AuthorizedUser extracts the user name and provider from context. If the
// user and provider can be found on the context, we look up the user by their
// name and provider. If the user is found, we verify that the user has at at
// least the role supplied.
func AuthorizedUser(
	store DataStore,
	useAuth bool,
	role string,
	logger chronograf.Logger,
	next http.HandlerFunc,
) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !useAuth {
			next(w, r)
			return
		}

		log := logger.
			WithField("component", "role_auth").
			WithField("remote_addr", r.RemoteAddr).
			WithField("method", r.Method).
			WithField("url", r.URL)

		ctx := r.Context()

		p, err := getValidPrincipal(ctx)
		if err != nil {
			log.Error("Failed to retrieve principal from context")
			Error(w, http.StatusUnauthorized, "User is not authorized", logger)
			return
		}
		scheme, err := getScheme(ctx)
		if err != nil {
			log.Error("Failed to retrieve scheme from context")
			Error(w, http.StatusUnauthorized, "User is not authorized", logger)
			return
		}

		if p.Organization == "" {
			p.Organization = "0"
		}

		// validate that the organization exists
		orgID, err := strconv.ParseUint(p.Organization, 10, 64)
		if err != nil {
			log.Error("Failed to validate organization on context")
			Error(w, http.StatusUnauthorized, "User is not authorized", logger)
			return
		}
		_, err = store.Organizations(ctx).Get(ctx, chronograf.OrganizationQuery{ID: &orgID})
		if err != nil {
			log.Error(fmt.Sprintf("Failed to retrieve organization %d from organizations store", orgID))
			Error(w, http.StatusUnauthorized, "User is not authorized", logger)
			return
		}

		ctx = context.WithValue(ctx, "organizationID", p.Organization)
		// TODO: add real implementation
		serverCtx := context.WithValue(ctx, "superadmin", true)
		// TODO: seems silly to look up a user twice
		u, err := store.Users(serverCtx).Get(serverCtx, chronograf.UserQuery{
			Name:     &p.Subject,
			Provider: &p.Issuer,
			Scheme:   &scheme,
		})

		if err != nil {
			log.Error("Failed to retrieve user")
			Error(w, http.StatusUnauthorized, "User is not authorized", logger)
			return
		}

		if u.SuperAdmin {
			// This context is where superadmin gets set for all things
			r = r.WithContext(serverCtx)
			next(w, r)
			return
		}

		u, err = store.Users(ctx).Get(ctx, chronograf.UserQuery{
			Name:     &p.Subject,
			Provider: &p.Issuer,
			Scheme:   &scheme,
		})
		if err != nil {
			log.Error("Failed to retrieve user")
			Error(w, http.StatusUnauthorized, "User is not authorized", logger)
			return
		}

		if hasAuthorizedRole(u, role) {
			next(w, r)
			return
		}

		Error(w, http.StatusUnauthorized, "User is not authorized", logger)
		return
	})
}

func hasAuthorizedRole(u *chronograf.User, role string) bool {
	if u == nil {
		return false
	}

	switch role {
	case ViewerRoleName:
		for _, r := range u.Roles {
			switch r.Name {
			case ViewerRoleName, EditorRoleName, AdminRoleName:
				return true
			}
		}
	case EditorRoleName:
		for _, r := range u.Roles {
			switch r.Name {
			case EditorRoleName, AdminRoleName:
				return true
			}
		}
	case AdminRoleName:
		for _, r := range u.Roles {
			switch r.Name {
			case AdminRoleName:
				return true
			}
		}
	case SuperAdminRoleName:
		// SuperAdmins should have been authorized before this.
		// This is only meant to restrict access for non-superadmins.
		return false
	}

	return false
}
