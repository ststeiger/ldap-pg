package main

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/jmoiron/sqlx/types"
	"golang.org/x/xerrors"
)

var (
	findByDNStmt               *sqlx.NamedStmt
	findByDNWithMemberOfStmt   *sqlx.NamedStmt
	findByDNWithLockStmt       *sqlx.NamedStmt
	findCredByDNStmt           *sqlx.NamedStmt
	findByMemberOfWithLockStmt *sqlx.NamedStmt
	findByMemberWithLockStmt   *sqlx.NamedStmt
	addStmt                    *sqlx.NamedStmt
	updateAttrsByIdStmt        *sqlx.NamedStmt
	updateDNByIdStmt           *sqlx.NamedStmt
	deleteByDNStmt             *sqlx.NamedStmt
)

// For generic filter
type FilterStmtMap struct {
	sm sync.Map
}

func (m *FilterStmtMap) Get(key string) (*sqlx.NamedStmt, bool) {
	val, ok := m.sm.Load(key)
	if !ok {
		return nil, false
	}
	return val.(*sqlx.NamedStmt), true
}

func (m *FilterStmtMap) Put(key string, value *sqlx.NamedStmt) {
	m.sm.Store(key, value)
}

var filterStmtMap FilterStmtMap

func initStmt(db *sqlx.DB) error {
	var err error

	findByDNSQL := "SELECT id, dn_norm, attrs_orig FROM ldap_entry WHERE dn_norm = :dnNorm"
	findByDNWithMemberOfSQL := "SELECT id, dn_norm, attrs_orig, (select jsonb_agg(e2.dn_norm) AS memberOf FROM ldap_entry e2 WHERE e2.attrs_norm->'member' @> jsonb_build_array(e1.dn_norm)) AS memberOf FROM ldap_entry e1 WHERE dn_norm = :dnNorm"

	findByDNStmt, err = db.PrepareNamed(findByDNSQL)
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	findByDNWithMemberOfStmt, err = db.PrepareNamed(findByDNWithMemberOfSQL)
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	findByDNWithLockStmt, err = db.PrepareNamed(findByDNSQL + " FOR UPDATE")
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	findCredByDNStmt, err = db.PrepareNamed("SELECT attrs_norm->>'userPassword' FROM ldap_entry WHERE dn_norm = :dnNorm")
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	findByMemberOfWithLockStmt, err = db.PrepareNamed(`SELECT id, dn_norm, attrs_orig FROM ldap_entry WHERE attrs_norm->'memberOf' @> jsonb_build_array(CAST(:dnNorm AS text)) FOR UPDATE`)
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	findByMemberWithLockStmt, err = db.PrepareNamed(`SELECT id, dn_norm, attrs_orig FROM ldap_entry WHERE attrs_norm->'member' @> jsonb_build_array(CAST(:dnNorm AS text)) FOR UPDATE`)
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	addStmt, err = db.PrepareNamed(`INSERT INTO ldap_entry (dn_norm, path, uuid, created, updated, attrs_norm, attrs_orig) SELECT :dnNorm, :path, :uuid, :created, :updated, :attrsNorm, :attrsOrig WHERE NOT EXISTS (SELECT id FROM ldap_entry WHERE dn_norm = :dnNorm) RETURNING id`)
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	updateAttrsByIdStmt, err = db.PrepareNamed(`UPDATE ldap_entry SET updated = now(), attrs_norm = :attrsNorm, attrs_orig = :attrsOrig WHERE id = :id`)
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	updateDNByIdStmt, err = db.PrepareNamed(`UPDATE ldap_entry SET updated = now(), dn_norm = :newdnNorm, path = :newpath, attrs_norm = :attrsNorm, attrs_orig = :attrsOrig WHERE id = :id`)
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	deleteByDNStmt, err = db.PrepareNamed(`DELETE FROM ldap_entry WHERE dn_norm = :dnNorm RETURNING id`)
	if err != nil {
		return xerrors.Errorf("Faild to initialize prepared statement: %w", err)
	}

	return nil
}

type FetchedDBEntry struct {
	Id        int64          `db:"id"`
	DNNorm    string         `db:"dn_norm"`
	AttrsOrig types.JSONText `db:"attrs_orig"`
	MemberOf  types.JSONText `db:"memberof"` // No real column in the table
	Count     int32          `db:"count"`    // No real column in the table
}

func (e *FetchedDBEntry) GetAttrsOrig() map[string][]string {
	if len(e.AttrsOrig) > 0 {
		jsonMap := make(map[string][]string)
		e.AttrsOrig.Unmarshal(&jsonMap)

		if len(e.MemberOf) > 0 {
			jsonArray := []string{}
			e.MemberOf.Unmarshal(&jsonArray)
			jsonMap["memberOf"] = jsonArray
		}

		return jsonMap
	}
	return nil
}

func (e *FetchedDBEntry) Clear() {
	e.Id = 0
	e.DNNorm = ""
	e.AttrsOrig = nil
	e.MemberOf = nil
	e.Count = 0
}

type DBEntry struct {
	Id            int64          `db:"id"`
	DNNorm        string         `db:"dn_norm"`
	Path          string         `db:"path"`
	EntryUUID     string         `db:"uuid"`
	Created       time.Time      `db:"created"`
	Updated       time.Time      `db:"updated"`
	AttrsNorm     types.JSONText `db:"attrs_norm"`
	AttrsOrig     types.JSONText `db:"attrs_orig"`
	Count         int32          `db:"count"`    // No real column in the table
	MemberOf      types.JSONText `db:"memberof"` // No real column in the table
	jsonAttrsNorm map[string]interface{}
	jsonAttrsOrig map[string][]string
}

func insert(tx *sqlx.Tx, entry *AddEntry) (int64, error) {
	dbEntry, err := mapper.AddEntryToDBEntry(entry)
	if err != nil {
		return 0, err
	}

	rows, err := tx.NamedStmt(addStmt).Queryx(map[string]interface{}{
		"dnNorm":    dbEntry.DNNorm,
		"path":      dbEntry.Path,
		"uuid":      dbEntry.EntryUUID,
		"created":   dbEntry.Created,
		"updated":   dbEntry.Updated,
		"attrsNorm": dbEntry.AttrsNorm,
		"attrsOrig": dbEntry.AttrsOrig,
	})
	if err != nil {
		log.Printf("error: Failed to insert entry record. entry: %#v err: %v", entry, err)
		return 0, err
	}

	var id int64
	if rows.Next() {
		rows.Scan(&id)
	} else {
		return 0, NewAlreadyExists()
	}

	return id, nil
}

func update(tx *sqlx.Tx, entry *ModifyEntry) error {
	if entry.dbEntryId == 0 {
		return fmt.Errorf("Invalid dbEntryId for update DBEntry.")
	}

	dbEntry, err := mapper.ModifyEntryToDBEntry(entry)
	if err != nil {
		return err
	}

	_, err = tx.NamedStmt(updateAttrsByIdStmt).Exec(map[string]interface{}{
		"id":        dbEntry.Id,
		"attrsNorm": dbEntry.AttrsNorm,
		"attrsOrig": dbEntry.AttrsOrig,
	})
	if err != nil {
		log.Printf("error: Failed to update entry record. entry: %#v err: %v", entry, err)
		return err
	}

	return nil
}

func updateDN(tx *sqlx.Tx, oldDN, newDN *DN) error {
	err := renameMemberByMemberDN(tx, oldDN, newDN)
	if err != nil {
		return xerrors.Errorf("Faild to rename member. err: %w", err)
	}

	oldEntry, err := findByDNWithLock(tx, oldDN)
	if err != nil {
		return err
	}

	newEntry := oldEntry.ModifyDN(newDN)
	dbEntry, err := mapper.ModifyEntryToDBEntry(newEntry)
	if err != nil {
		return err
	}

	_, err = tx.NamedStmt(updateDNByIdStmt).Exec(map[string]interface{}{
		"id":        newEntry.dbEntryId,
		"newdnNorm": newDN.DNNorm,
		"newpath":   newDN.ReverseParentDN,
		"attrsNorm": dbEntry.AttrsNorm,
		"attrsOrig": dbEntry.AttrsOrig,
	})

	if err != nil {
		if strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
			log.Printf("warn: Failed to update entry DN because of already exists. oldDN: %s newDN: %s err: %v", oldDN.DNNorm, newDN.DNNorm, err)
			return NewAlreadyExists()
		}
		return xerrors.Errorf("Faild to update entry DN. oldDN: %s, newDN: %s, err: %w", oldDN.DNNorm, newDN.DNNorm, err)
	}

	return nil
}

func renameMemberByMemberDN(tx *sqlx.Tx, oldMemberDN, newMemberDN *DN) error {
	// We need to fetch all rows and close before updating due to avoiding "pq: unexpected Parse response" error.
	// https://github.com/lib/pq/issues/635
	modifyEntries, err := findByMemberDNWithLock(tx, oldMemberDN)
	if err != nil {
		return err
	}

	if len(modifyEntries) == 0 {
		log.Printf("No entries which have member for rename. memberDN: %s", oldMemberDN.DNNorm)
		return nil
	}

	for _, modifyEntry := range modifyEntries {
		modifyEntry.Delete("member", []string{oldMemberDN.DNOrig})
		modifyEntry.Add("member", []string{newMemberDN.DNOrig})

		err := update(tx, modifyEntry)
		if err != nil {
			return err
		}
	}
	return nil
}

func deleteByDN(tx *sqlx.Tx, dn *DN) error {
	err := deleteMemberByMemberDN(tx, dn)
	if err != nil {
		return xerrors.Errorf("Faild to delete member. err: %w", err)
	}

	var id int = 0
	err = tx.NamedStmt(deleteByDNStmt).Get(&id, map[string]interface{}{
		"dnNorm": dn.DNNorm,
	})
	if err != nil {
		if strings.Contains(err.Error(), "sql: no rows in result set") {
			return NewNoSuchObject()
		}
		return xerrors.Errorf("Faild to delete entry. dn: %s, err: %w", dn.DNNorm, err)
	}
	if id == 0 {
		return NewNoSuchObject()
	}
	return nil
}

func deleteMemberByMemberDN(tx *sqlx.Tx, memberDN *DN) error {
	// We need to fetch all rows and close before updating due to avoiding "pq: unexpected Parse response" error.
	// https://github.com/lib/pq/issues/635
	modifyEntries, err := findByMemberDNWithLock(tx, memberDN)
	if err != nil {
		return err
	}

	if len(modifyEntries) == 0 {
		log.Printf("No entries which have member for delete. memberDN: %s", memberDN.DNNorm)
		return nil
	}

	for _, modifyEntry := range modifyEntries {
		modifyEntry.Delete("member", []string{memberDN.DNOrig})

		err := update(tx, modifyEntry)
		if err != nil {
			return err
		}
	}
	return nil
}

func findByMemberDNWithLock(tx *sqlx.Tx, memberDN *DN) ([]*ModifyEntry, error) {
	rows, err := tx.NamedStmt(findByMemberWithLockStmt).Queryx(map[string]interface{}{
		"dnNorm": memberDN.DNNorm,
	})
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dbEntry := FetchedDBEntry{}
	modifyEntries := []*ModifyEntry{}

	for rows.Next() {
		err := rows.StructScan(&dbEntry)
		if err != nil {
			return nil, err
		}
		modifyEntry, err := mapper.FetchedDBEntryToModifyEntry(&dbEntry)
		if err != nil {
			return nil, err
		}

		modifyEntries = append(modifyEntries, modifyEntry)

		dbEntry.Clear()
	}

	err = rows.Err()
	if err != nil {
		return nil, err
	}

	return modifyEntries, nil
}

func findByDN(tx *sqlx.Tx, dn *DN) (*SearchEntry, error) {
	dbEntry := FetchedDBEntry{}
	err := tx.NamedStmt(findByDNStmt).Get(&dbEntry, map[string]interface{}{
		"dnNorm": dn.DNNorm,
	})
	if err != nil {
		return nil, err
	}
	dbEntry.Count = 1
	return mapper.FetchedDBEntryToSearchEntry(&dbEntry)
}

func findByDNWithLock(tx *sqlx.Tx, dn *DN) (*ModifyEntry, error) {
	return findByDNNormWithLock(tx, dn.DNNorm)
}

func findByDNNormWithLock(tx *sqlx.Tx, dnNorm string) (*ModifyEntry, error) {
	dbEntry := FetchedDBEntry{}
	err := tx.NamedStmt(findByDNWithLockStmt).Get(&dbEntry, map[string]interface{}{
		"dnNorm": dnNorm,
	})
	if err != nil {
		return nil, err
	}
	dbEntry.Count = 1
	return mapper.FetchedDBEntryToModifyEntry(&dbEntry)
}

func findCredByDN(dn *DN) (string, error) {
	var bindUserCred string
	err := findCredByDNStmt.Get(&bindUserCred, map[string]interface{}{
		"dnNorm": dn.DNNorm,
	})
	if err != nil {
		return "", err
	}
	return bindUserCred, nil
}

func findByFilter(pathQuery string, q *Query, reqMemberOf bool, handler func(entry *SearchEntry) error) (int32, int32, error) {
	var query string
	if q.Query != "" {
		query = " AND " + q.Query
	}

	var fetchQuery string
	if reqMemberOf {
		fetchQuery = fmt.Sprintf(`SELECT id, dn_norm, attrs_orig, (select jsonb_agg(e2.dn_norm) AS memberOf FROM ldap_entry e2 WHERE e2.attrs_norm->'member' @> jsonb_build_array(e1.dn_norm)) AS memberOf, count(id) over() AS count FROM ldap_entry e1 WHERE %s %s LIMIT :pageSize OFFSET :offset`, pathQuery, query)
	} else {
		fetchQuery = fmt.Sprintf(`SELECT id, dn_norm, attrs_orig, count(id) over() AS count FROM ldap_entry WHERE %s %s LIMIT :pageSize OFFSET :offset`, pathQuery, query)
	}

	log.Printf("Fetch Query: %s Params: %v", fetchQuery, q.Params)

	var fetchStmt *sqlx.NamedStmt
	var ok bool
	var err error
	if fetchStmt, ok = filterStmtMap.Get(fetchQuery); !ok {
		// cache
		fetchStmt, err = db.PrepareNamed(fetchQuery)
		if err != nil {
			return 0, 0, err
		}
		filterStmtMap.Put(fetchQuery, fetchStmt)
	}

	var rows *sqlx.Rows
	rows, err = fetchStmt.Queryx(q.Params)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()

	dbEntry := FetchedDBEntry{}
	var maxCount int32 = 0
	var count int32 = 0

	for rows.Next() {
		err := rows.StructScan(&dbEntry)
		if err != nil {
			log.Printf("error: DBEntry struct mapping error: %#v", err)
			return 0, 0, err
		}

		readEntry, err := mapper.FetchedDBEntryToSearchEntry(&dbEntry)
		if err != nil {
			log.Printf("error: Mapper error: %#v", err)
			return 0, 0, err
		}

		if maxCount == 0 {
			maxCount = dbEntry.Count
		}

		err = handler(readEntry)
		if err != nil {
			log.Printf("error: Handler error: %#v", err)
			return 0, 0, err
		}

		count++
		dbEntry.Clear()
	}

	err = rows.Err()
	if err != nil {
		log.Printf("error: Search error: %#v", err)
		return 0, 0, err
	}

	return maxCount, count, nil
}
