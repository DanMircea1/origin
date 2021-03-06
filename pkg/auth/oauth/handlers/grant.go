package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/RangelReale/osin"

	"k8s.io/kubernetes/pkg/auth/user"

	"github.com/openshift/origin/pkg/auth/api"
	oauthapi "github.com/openshift/origin/pkg/oauth/api"
)

// GrantCheck implements osinserver.AuthorizeHandler to ensure requested scopes have been authorized
type GrantCheck struct {
	check        GrantChecker
	handler      GrantHandler
	errorHandler GrantErrorHandler
}

// NewGrantCheck returns a new GrantCheck
func NewGrantCheck(check GrantChecker, handler GrantHandler, errorHandler GrantErrorHandler) *GrantCheck {
	return &GrantCheck{check, handler, errorHandler}
}

// HandleAuthorize implements osinserver.AuthorizeHandler to ensure the requested scopes have been authorized.
// The AuthorizeRequest.Authorized field must already be set to true for the grant check to occur.
// If the requested scopes are authorized, the AuthorizeRequest is unchanged.
// If the requested scopes are not authorized, or an error occurs, AuthorizeRequest.Authorized is set to false.
// If the response is written, true is returned.
// If the response is not written, false is returned.
func (h *GrantCheck) HandleAuthorize(ar *osin.AuthorizeRequest, w http.ResponseWriter) (bool, error) {

	// Requests must already be authorized before we will check grants
	if !ar.Authorized {
		return false, nil
	}

	// Reset request to unauthorized until we verify the grant
	ar.Authorized = false

	user, ok := ar.UserData.(user.Info)
	if !ok || user == nil {
		return h.errorHandler.GrantError(errors.New("the provided user data is not user.Info"), w, ar.HttpRequest)
	}

	grant := &api.Grant{
		Client:      ar.Client,
		Scope:       ar.Scope,
		Expiration:  int64(ar.Expiration),
		RedirectURI: ar.RedirectUri,
	}

	// Check if the user has already authorized this grant
	authorized, err := h.check.HasAuthorizedClient(user, grant)
	if err != nil {
		return h.errorHandler.GrantError(err, w, ar.HttpRequest)
	}
	if authorized {
		ar.Authorized = true
		return false, nil
	}

	// React to an unauthorized grant
	authorized, handled, err := h.handler.GrantNeeded(user, grant, w, ar.HttpRequest)
	if authorized {
		ar.Authorized = true
	}
	return handled, err
}

type emptyGrant struct{}

// NewEmptyGrant returns a no-op grant handler
func NewEmptyGrant() GrantHandler {
	return emptyGrant{}
}

// GrantNeeded implements the GrantHandler interface
func (emptyGrant) GrantNeeded(user user.Info, grant *api.Grant, w http.ResponseWriter, req *http.Request) (bool, bool, error) {
	return false, false, nil
}

type autoGrant struct {
}

// NewAutoGrant returns a grant handler that automatically approves client authorizations
func NewAutoGrant() GrantHandler {
	return &autoGrant{}
}

// GrantNeeded implements the GrantHandler interface
func (g *autoGrant) GrantNeeded(user user.Info, grant *api.Grant, w http.ResponseWriter, req *http.Request) (bool, bool, error) {
	return true, false, nil
}

type redirectGrant struct {
	url string
}

// NewRedirectGrant returns a grant handler that redirects to the given URL when a grant is needed.
// The following query parameters are added to the URL:
//   then - original request URL
//   client_id - requesting client's ID
//   scopes - grant scope requested
//   redirect_uri - original authorize request redirect_uri
func NewRedirectGrant(url string) GrantHandler {
	return &redirectGrant{url}
}

// GrantNeeded implements the GrantHandler interface
func (g *redirectGrant) GrantNeeded(user user.Info, grant *api.Grant, w http.ResponseWriter, req *http.Request) (bool, bool, error) {
	redirectURL, err := url.Parse(g.url)
	if err != nil {
		return false, false, err
	}
	redirectURL.RawQuery = url.Values{
		"then":         {req.URL.String()},
		"client_id":    {grant.Client.GetId()},
		"scopes":       {grant.Scope},
		"redirect_uri": {grant.RedirectURI},
	}.Encode()
	http.Redirect(w, req, redirectURL.String(), http.StatusFound)
	return false, true, nil
}

type perClientGrant struct {
	auto          GrantHandler
	prompt        GrantHandler
	deny          GrantHandler
	defaultMethod oauthapi.GrantHandlerType
}

// NewPerClientGrant returns a grant handler that determines what to do based on the grant method in the client
func NewPerClientGrant(prompt GrantHandler, defaultMethod oauthapi.GrantHandlerType) GrantHandler {
	return &perClientGrant{
		auto:          NewAutoGrant(),
		prompt:        prompt,
		deny:          NewEmptyGrant(),
		defaultMethod: defaultMethod,
	}
}

func (g *perClientGrant) GrantNeeded(user user.Info, grant *api.Grant, w http.ResponseWriter, req *http.Request) (bool, bool, error) {
	client, ok := grant.Client.GetUserData().(*oauthapi.OAuthClient)
	if !ok {
		return false, false, errors.New("unrecognized OAuth client type")
	}

	method := client.GrantMethod
	if len(method) == 0 {
		// Use the global default
		method = g.defaultMethod
	}

	switch method {
	case oauthapi.GrantHandlerAuto:
		return g.auto.GrantNeeded(user, grant, w, req)

	case oauthapi.GrantHandlerPrompt:
		return g.prompt.GrantNeeded(user, grant, w, req)

	case oauthapi.GrantHandlerDeny:
		return g.deny.GrantNeeded(user, grant, w, req)

	default:
		return false, false, fmt.Errorf("OAuth client grant method %q unrecognized", method)
	}
}
