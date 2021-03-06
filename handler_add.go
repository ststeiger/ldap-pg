package main

import (
	"log"

	ldap "github.com/openstandia/ldapserver"
	"golang.org/x/xerrors"
)

func handleAdd(s *Server, w ldap.ResponseWriter, m *ldap.Message) {
	r := m.GetAddRequest()

	dn, err := s.NormalizeDN(string(r.Entry()))
	if err != nil {
		log.Printf("warn: Invalid DN: %s err: %s", r.Entry(), err)

		responseAddError(w, err)
		return
	}

	log.Printf("debug: Adding Internal DN: %v", dn)

	if !requiredAuthz(m, "add", dn) {
		responseAddError(w, NewInsufficientAccess())
		return
	}

	addEntry, err := mapper.LDAPMessageToAddEntry(dn, r.Attributes())
	if err != nil {
		responseAddError(w, err)
		return
	}

	log.Printf("info: Adding entry: %s", r.Entry())

	id, err := s.Repo().Insert(addEntry)
	if err != nil {
		responseAddError(w, err)
		return
	}

	log.Printf("Added. Id: %d", id)

	res := ldap.NewAddResponse(ldap.LDAPResultSuccess)
	w.Write(res)

	log.Printf("info: End Adding entry: %s", r.Entry())
}

func responseAddError(w ldap.ResponseWriter, err error) {
	var ldapErr *LDAPError
	if ok := xerrors.As(err, &ldapErr); ok {
		log.Printf("warn: Add LDAP error. err: %+v", err)

		res := ldap.NewAddResponse(ldapErr.Code)
		if ldapErr.Msg != "" {
			res.SetDiagnosticMessage(ldapErr.Msg)
		}
		if ldapErr.MatchedDN != "" {
			res.SetMatchedDN(ldapErr.MatchedDN)
		}
		w.Write(res)
	} else {
		log.Printf("error: Add error. err: %+v", err)

		// TODO
		res := ldap.NewAddResponse(ldap.LDAPResultProtocolError)
		w.Write(res)
	}
}
