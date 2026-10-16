package server

import (
	"context"
	"net/http"

	"github.com/compliance-framework/factory/internal/apiserver/gen"
	"github.com/compliance-framework/factory/internal/auth"
)

// redirect is a 302 with cookies: the generated 302 responses carry one Set-Cookie at most,
// and the callback sets the session and clears the state.
type redirect struct {
	location string
	cookies  []*http.Cookie
}

func (r redirect) visit(w http.ResponseWriter) error {
	for _, c := range r.cookies {
		http.SetCookie(w, c)
	}
	w.Header().Set("Location", r.location)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
	return nil
}

// VisitLoginResponse implements gen.LoginResponseObject.
func (r redirect) VisitLoginResponse(w http.ResponseWriter) error { return r.visit(w) }

// VisitAuthCallbackResponse implements gen.AuthCallbackResponseObject.
func (r redirect) VisitAuthCallbackResponse(w http.ResponseWriter) error { return r.visit(w) }

// loggedOut is logout's 204 with the cookie that clears the session.
type loggedOut struct{ cookie *http.Cookie }

// VisitLogoutResponse implements gen.LogoutResponseObject.
func (l loggedOut) VisitLogoutResponse(w http.ResponseWriter) error {
	http.SetCookie(w, l.cookie)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// Login implements gen.StrictServerInterface: a redirect to GitHub with a state bound to a
// short-lived cookie.
func (s *Server) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	returnTo := ""
	if req.Params.ReturnTo != nil {
		returnTo = *req.Params.ReturnTo
	}
	loc, cookie, err := s.d.Auth.BeginLogin(ctx, returnTo)
	if err != nil {
		return nil, err
	}
	return redirect{location: loc, cookies: []*http.Cookie{cookie}}, nil
}

// AuthCallback implements gen.StrictServerInterface: checks the state against its cookie,
// exchanges the code, sets the session cookie and redirects to the returnTo path.
func (s *Server) AuthCallback(ctx context.Context, req gen.AuthCallbackRequestObject) (gen.AuthCallbackResponseObject, error) {
	l, err := s.d.Auth.FinishLogin(ctx, requestFrom(ctx), req.Params.Code, req.Params.State)
	if err != nil {
		return nil, err
	}
	return redirect{location: l.ReturnTo, cookies: []*http.Cookie{l.Session, l.ClearState}}, nil
}

// Logout implements gen.StrictServerInterface. It succeeds without a session too.
func (s *Server) Logout(ctx context.Context, _ gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	c, err := s.d.Auth.Logout(ctx, requestFrom(ctx))
	if err != nil {
		return nil, err
	}
	return loggedOut{cookie: c}, nil
}

// GetMe implements gen.StrictServerInterface.
func (s *Server) GetMe(ctx context.Context, _ gen.GetMeRequestObject) (gen.GetMeResponseObject, error) {
	p := principal(ctx)
	if p.Kind != auth.KindUser {
		return nil, problemf(http.StatusUnauthorized, CodeUnauthenticated, "no session")
	}
	isAdmin, err := s.d.Auth.IsAdmin(ctx, p.User)
	if err != nil {
		return nil, err
	}
	me := gen.Me{Login: p.User.Login, IsAdmin: isAdmin}
	if p.User.Name != "" {
		n := p.User.Name
		me.Name = &n
	}
	if p.User.AvatarURL != "" {
		a := p.User.AvatarURL
		me.AvatarUrl = &a
	}
	return gen.GetMe200JSONResponse(me), nil
}

// GetDeviceConfig implements gen.StrictServerInterface.
func (s *Server) GetDeviceConfig(_ context.Context, _ gen.GetDeviceConfigRequestObject) (gen.GetDeviceConfigResponseObject, error) {
	id, scopes := s.d.Auth.DeviceConfig()
	if id == "" {
		return nil, problemf(http.StatusServiceUnavailable, CodeUnavailable, "no OAuth client is configured")
	}
	return gen.GetDeviceConfig200JSONResponse{ClientId: id, Scopes: scopes}, nil
}
