package contacts

import (
	"context"
	"fmt"

	"github.com/keybase/client/go/encrypteddb"

	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/protocol/keybase1"
)

// Saving contact list into encrypted db.

// Cache resolutions of a lookup ran on entire contact list provided by the
// frontend. Assume every time SaveContacts is called, entire contact list is
// passed as an argument. Always cache the result of last resolution, do not do
// any result merging.

type SavedContactsStore struct {
	encryptedDB *encrypteddb.EncryptedDB
}

var _ libkb.SyncedContactListProvider = (*SavedContactsStore)(nil)

// NewSavedContactsStore creates a new SavedContactsStore for global context.
// The store is used to securely store list of resolved contacts.
func NewSavedContactsStore(g *libkb.GlobalContext) *SavedContactsStore {
	keyFn := func(ctx context.Context) ([32]byte, error) {
		return encrypteddb.GetSecretBoxKey(ctx, g, encrypteddb.DefaultSecretUI,
			libkb.EncryptionReasonContactsLocalStorage, "encrypting local contact list")
	}
	dbFn := func(g *libkb.GlobalContext) *libkb.JSONLocalDb {
		return g.LocalDb
	}
	return &SavedContactsStore{
		encryptedDB: encrypteddb.New(g, dbFn, keyFn),
	}
}

func ServiceInit(g *libkb.GlobalContext) {
	g.SyncedContactList = NewSavedContactsStore(g)
}

func savedContactsDbKey(uid keybase1.UID) libkb.DbKey {
	return libkb.DbKey{
		Typ: libkb.DBSavedContacts,
		Key: fmt.Sprintf("%v", uid),
	}
}

type savedContactsCache struct {
	Contacts []keybase1.ProcessedContact
	Version  int
}

const savedContactsCurrentVer = 1

func assertionToNameDbKey(uid keybase1.UID) libkb.DbKey {
	return libkb.DbKey{
		Typ: libkb.DBSavedContacts,
		Key: fmt.Sprintf("lookup:%v", uid),
	}
}

type assertionToNameCache struct {
	AssertionToName map[string]string
	Version         int
}

const assertionToNameCurrentVer = 1

func ResolveAndSaveContacts(mctx libkb.MetaContext, provider ContactsProvider, contacts []keybase1.Contact) (newlyResolved []keybase1.ProcessedContact, err error) {
	resolveResults, err := ResolveContacts(mctx, provider, contacts, keybase1.RegionCode(""))
	if err != nil {
		return nil, err
	}

	// find newly resolved
	s := mctx.G().SyncedContactList
	currentContacts, err := s.RetrieveContacts(mctx)

	newlyResolvedMap := make(map[string]keybase1.ProcessedContact)
	if err == nil {
		unres := make(map[string]struct{})
		for _, contact := range currentContacts {
			if !contact.Resolved {
				// We resolve based on display name, not assertion, so we don't
				// duplicate multiple assertions for the same contact.
				unres[contact.ContactName] = struct{}{}
			}
		}

		for _, resolution := range resolveResults {
			if _, wasUnresolved := unres[resolution.ContactName]; wasUnresolved && resolution.Resolved {
				// We only want to show one resolution per username.
				newlyResolvedMap[resolution.Username] = resolution
			}
		}
	} else {
		mctx.Warning("error retrieving synced contacts; continuing: %s", err)
	}

	if len(newlyResolvedMap) == 0 {
		return newlyResolved, s.SaveProcessedContacts(mctx, resolveResults)
	}

	resolutionsForPeoplePage := make([]ContactResolution, 0, len(newlyResolvedMap))
	for _, contact := range newlyResolvedMap {
		resolutionsForPeoplePage = append(resolutionsForPeoplePage, ContactResolution{
			Description: fmt.Sprintf("%s — %s", contact.ContactName,
				contact.Component.ValueString()),
			ResolvedUser: keybase1.User{
				Uid:      contact.Uid,
				Username: contact.Username,
			},
		})
		newlyResolved = append(newlyResolved, contact)
	}
	err = SendEncryptedContactResolutionToServer(mctx, resolutionsForPeoplePage)
	if err != nil {
		mctx.Warning("Could not add resolved contacts to people page: %v; returning contacts anyway", err)
	}

	return newlyResolved, s.SaveProcessedContacts(mctx, resolveResults)
}

func makeAssertionToName(contacts []keybase1.ProcessedContact) (res map[string]string) {
	res = make(map[string]string)
	toRemove := make(map[string]struct{})
	for _, contact := range contacts {
		if _, ok := res[contact.Assertion]; ok {
			// multiple contacts match this assertion, remove once we're done
			toRemove[contact.Assertion] = struct{}{}
			continue
		}
		res[contact.Assertion] = contact.ContactName
	}
	for remove := range toRemove {
		delete(res, remove)
	}
	return res
}

func (s *SavedContactsStore) SaveProcessedContacts(mctx libkb.MetaContext, contacts []keybase1.ProcessedContact) (err error) {
	val := savedContactsCache{
		Contacts: contacts,
		Version:  savedContactsCurrentVer,
	}

	cacheKey := savedContactsDbKey(mctx.CurrentUID())
	err = s.encryptedDB.Put(mctx.Ctx(), cacheKey, val)
	if err != nil {
		return err
	}

	assertionToName := makeAssertionToName(contacts)
	lookupVal := assertionToNameCache{
		AssertionToName: assertionToName,
		Version:         assertionToNameCurrentVer,
	}

	cacheKey = assertionToNameDbKey(mctx.CurrentUID())
	err = s.encryptedDB.Put(mctx.Ctx(), cacheKey, lookupVal)
	return err
}

func (s *SavedContactsStore) RetrieveContacts(mctx libkb.MetaContext) (ret []keybase1.ProcessedContact, err error) {
	cacheKey := savedContactsDbKey(mctx.CurrentUID())
	var cache savedContactsCache
	found, err := s.encryptedDB.Get(mctx.Ctx(), cacheKey, &cache)
	if err != nil {
		return nil, err
	}
	if !found {
		return ret, nil
	}
	if cache.Version != savedContactsCurrentVer {
		mctx.Warning("synced contact list found but had an old version (found: %d, need: %d), returning empty list",
			cache.Version, savedContactsCurrentVer)
		return ret, nil
	}
	return cache.Contacts, nil
}

func (s *SavedContactsStore) RetrieveAssertionToName(mctx libkb.MetaContext) (ret map[string]string, err error) {
	cacheKey := assertionToNameDbKey(mctx.CurrentUID())
	var cache assertionToNameCache
	found, err := s.encryptedDB.Get(mctx.Ctx(), cacheKey, &cache)
	if err != nil {
		return nil, err
	}
	if !found {
		return ret, nil
	}
	if cache.Version != assertionToNameCurrentVer {
		mctx.Warning("assertion to name found but had an old version (found: %d, need: %d), returning empty map",
			cache.Version, assertionToNameCurrentVer)
		return ret, nil
	}
	return cache.AssertionToName, nil
}

func (s *SavedContactsStore) UnresolveContactsWithComponent(mctx libkb.MetaContext,
	phoneNumber *keybase1.PhoneNumber, email *keybase1.EmailAddress) {
	// TODO: Use a phoneNumber | email variant instead of two pointers.
	contactList, err := s.RetrieveContacts(mctx)
	if err != nil {
		mctx.Warning("Failed to get cached contact list: %x", err)
		return
	}
	for i, con := range contactList {
		var unresolve bool
		switch {
		case phoneNumber != nil && con.Component.PhoneNumber != nil:
			unresolve = *con.Component.PhoneNumber == keybase1.RawPhoneNumber(*phoneNumber)
		case email != nil && con.Component.Email != nil:
			unresolve = *con.Component.Email == *email
		}

		if unresolve {
			// Unresolve contact.
			con.Resolved = false
			con.Username = ""
			con.Uid = ""
			con.Following = false
			con.FullName = ""
			// TODO: DisplayName/DisplayLabel logic infects yet another file /
			// module. But it will sort itself out once we get rid of both.
			con.DisplayName = con.ContactName
			con.DisplayLabel = con.Component.FormatDisplayLabel(false /* addLabel */)
			contactList[i] = con
		}
	}
	err = s.SaveProcessedContacts(mctx, contactList)
	if err != nil {
		mctx.Warning("Failed to put cached contact list: %x", err)
	}
}
