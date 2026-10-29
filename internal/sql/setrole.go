package sql

import "strings"

// SET ROLE / RESET ROLE / SET SESSION AUTHORIZATION and the names
// current_user, session_user and current_role answer (docs/05).
//
// A session has a session user (the login, changed by SET SESSION
// AUTHORIZATION) and a current user (SET ROLE; the session user when none).
// Privileges are checked for the current user (Session.role, "" = the
// bootstrap superuser). A session that logged in with the shared server
// password has no stored role, so current_user and session_user answer the
// user name of the startup packet, as PostgreSQL does for a connection by
// that name, not the owner's name "stask".

type roleCtx struct {
	init      bool
	sessRole  string // role of the session user ("" = bootstrap superuser)
	sessName  string
	loginRole string
	loginName string
	curName   string // SET ROLE name; "" = none
}

// loginDisplay is the name the login is known by.
func (s *Session) loginDisplay() string {
	switch {
	case s.rc.init:
		return s.rc.loginName
	case s.role != "":
		return s.role
	case s.act != nil && s.act.user != "":
		return s.act.user
	}
	return roleName
}

func (s *Session) initRC() {
	if s.rc.init {
		return
	}
	name := s.loginDisplay()
	s.rc = roleCtx{init: true, sessRole: s.role, sessName: name, loginRole: s.role, loginName: name}
}

// sessionUserName is session_user.
func (s *Session) sessionUserName() string {
	if s.rc.init {
		return s.rc.sessName
	}
	return s.loginDisplay()
}

// currentUserName is current_user and current_role.
func (s *Session) currentUserName() string {
	if s.rc.init && s.rc.curName != "" {
		return s.rc.curName
	}
	return s.sessionUserName()
}

// sqlUser answers current_user / session_user / current_role.
func (s *Session) sqlUser(fn string) string {
	if fn == "session_user" {
		return s.sessionUserName()
	}
	return s.currentUserName()
}

func (s *Session) roleIsSuper(role string) bool {
	if role == "" {
		return true
	}
	r, ok := s.E.Role(role)
	return ok && r.Super
}

// roleTarget resolves a role name to the stored role ("" = bootstrap).
func (s *Session) roleTarget(name string) (role string, err error) {
	if name == roleName {
		return "", nil
	}
	if _, ok := s.E.Role(name); !ok {
		return "", errf("22023", "role %q does not exist", name)
	}
	return name, nil
}

func (s *Session) setRoleVars() {
	cur := "none"
	if s.rc.curName != "" {
		cur = s.rc.curName
	}
	s.vars["role"] = cur
	s.vars["session_authorization"] = s.sessionUserName()
}

// initUserVars is called when the session is tracked (its user is known).
func (s *Session) initUserVars() { s.setRoleVars() }

// setRoleVar handles SET [LOCAL] ROLE and SET SESSION AUTHORIZATION; ok is
// false for any other variable.
func (s *Session) setRoleVar(st *SetVar) (*Result, bool, error) {
	name := strings.ToLower(st.Name)
	if name != "role" && name != "authorization" && name != "session_authorization" {
		return nil, false, nil
	}
	val := strings.Trim(strings.TrimSpace(st.Value), "'\"")
	lower := strings.ToLower(val)
	if st.Local && s.explicit {
		s.initRC()
		s.setRoleVars()
		s.noteLocal("role")
		if name != "role" {
			s.noteLocal("session_authorization")
		}
	}
	s.initRC()
	defer s.setRoleVars()
	if name == "role" {
		if lower == "none" || lower == "default" || val == "" {
			s.role, s.rc.curName = s.rc.sessRole, ""
			return &Result{Tag: "SET"}, true, nil
		}
		if val == s.rc.sessName { // the session user itself: always allowed
			s.role, s.rc.curName = s.rc.sessRole, ""
			return &Result{Tag: "SET"}, true, nil
		}
		target, err := s.roleTarget(val)
		if err != nil {
			return nil, true, err
		}
		if !s.roleIsSuper(s.rc.sessRole) {
			return nil, true, errf("42501", "permission denied to set role %q", val)
		}
		s.role, s.rc.curName = target, val
		return &Result{Tag: "SET"}, true, nil
	}
	// SET SESSION AUTHORIZATION
	if lower == "default" || lower == "none" || val == "" || val == s.rc.loginName {
		s.rc.sessRole, s.rc.sessName = s.rc.loginRole, s.rc.loginName
		s.role, s.rc.curName = s.rc.loginRole, ""
		return &Result{Tag: "SET"}, true, nil
	}
	target, err := s.roleTarget(val)
	if err != nil {
		return nil, true, err
	}
	if !s.roleIsSuper(s.rc.loginRole) {
		return nil, true, errf("42501", "permission denied to set session authorization")
	}
	s.rc.sessRole, s.rc.sessName = target, val
	s.role, s.rc.curName = target, ""
	return &Result{Tag: "SET"}, true, nil
}

// resetRole is DISCARD ALL / RESET ALL for the role state.
func (s *Session) resetRole() {
	if s.rc.init {
		s.rc.sessRole, s.rc.sessName = s.rc.loginRole, s.rc.loginName
		s.role, s.rc.curName = s.rc.loginRole, ""
		s.setRoleVars()
	}
}
