package rapidpro

import (
	"database/sql"
	"fmt"

	null "gopkg.in/guregu/null.v3"

	"github.com/jmoiron/sqlx"
	"github.com/nyaruka/courier"
)

// ContactURNID represents a contact urn's id
type ContactURNID struct {
	null.Int
}

// NilContactURNID is our nil value for ContactURNID
var NilContactURNID = ContactURNID{null.NewInt(0, false)}

// NewDBContactURN returns a new ContactURN object for the passed in org, contact and string urn, this is not saved to the DB yet
func newDBContactURN(org OrgID, channelID courier.ChannelID, contactID ContactID, urn courier.URN) *DBContactURN {
	return &DBContactURN{
		OrgID:     org,
		ChannelID: channelID,
		ContactID: contactID,
		Identity:  urn.Identity(),
		Scheme:    urn.Scheme(),
		Path:      urn.Path(),
		Display:   urn.Display(),
	}
}

const selectContactURNs = `
SELECT id, identity, scheme, display, priority, contact_id, channel_id
FROM contacts_contacturn
WHERE contact_id = $1
ORDER BY priority desc
`

// selectContactURNs returns all the ContactURNs for the passed in contact, sorted by priority
func contactURNsForContact(db *sqlx.DB, contactID ContactID) ([]*DBContactURN, error) {
	// select all the URNs for this contact
	rows, err := db.Queryx(selectContactURNs, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// read our URNs out
	urns := make([]*DBContactURN, 0, 3)
	idx := 0
	for rows.Next() {
		u := &DBContactURN{}
		err = rows.StructScan(u)
		if err != nil {
			return nil, err
		}
		urns = append(urns, u)
		idx++
	}
	return urns, nil
}

// setDefaultURN makes sure that the passed in URN is the default URN for this contact and
// that the passed in channel is the default one for that URN
//
// Note that the URN must be one of the contact's URN before calling this method
func setDefaultURN(db *sqlx.DB, channelID courier.ChannelID, contact *DBContact, urn courier.URN) error {
	scheme := urn.Scheme()
	urns, err := contactURNsForContact(db, contact.ID)
	if err != nil {
		return err
	}

	// no URNs? that's an error
	if len(urns) == 0 {
		return fmt.Errorf("URN '%s' not present for contact %d", urn.Identity(), contact.ID.Int64)
	}

	// only a single URN and it is ours
	if urns[0].Identity == urn.Identity() {
		// if display or channel ids changed, update them
		if urns[0].Display != urn.Display() || urns[0].ChannelID != channelID {
			urns[0].Display = urn.Display()
			urns[0].ChannelID = channelID
			return updateContactURN(db, urns[0])
		}
		return nil
	}

	// multiple URNs and we aren't the top, iterate across them and update channel for matching schemes
	// this is kinda expensive (n SQL queries) but only happens for cases where there are multiple URNs for a contact (rare) and
	// the preferred channel changes (rare as well)
	topPriority := 99
	currPriority := 50
	for _, existing := range urns {
		if existing.Identity == urn.Identity() {
			existing.Priority = topPriority
			existing.ChannelID = channelID
		} else {
			existing.Priority = currPriority

			// if this is a phone number and we just received a message on a tel scheme, set that as our new preferred channel
			if existing.Scheme == courier.TelScheme && scheme == courier.TelScheme {
				existing.ChannelID = channelID
			}
			currPriority--
		}
		err := updateContactURN(db, existing)
		if err != nil {
			return err
		}
	}

	return nil
}

const selectOrgURN = `
SELECT org_id, id, identity, scheme, path, display, priority, channel_id, contact_id 
FROM contacts_contacturn
WHERE org_id = $1 AND identity = $2
ORDER BY priority desc LIMIT 1
`

// contactURNForURN returns the ContactURN for the passed in org and URN, creating and associating
// it with the passed in contact if necessary
func contactURNForURN(db *sqlx.DB, org OrgID, channelID courier.ChannelID, contactID ContactID, urn courier.URN) (*DBContactURN, error) {
	contactURN := newDBContactURN(org, channelID, contactID, urn)
	err := db.Get(contactURN, selectOrgURN, org, urn.Identity())
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	// we didn't find it, let's insert it
	if err == sql.ErrNoRows {
		err = insertContactURN(db, contactURN)
		if err != nil {
			return nil, err
		}
	}

	// make sure our contact URN is up to date
	if contactURN.ChannelID != channelID || contactURN.ContactID != contactID || contactURN.Display != urn.Display() {
		contactURN.ChannelID = channelID
		contactURN.ContactID = contactID
		contactURN.Display = urn.Display()
		err = updateContactURN(db, contactURN)
	}

	return contactURN, err
}

const insertURN = `
INSERT INTO contacts_contacturn(org_id, identity, path, scheme, display, priority, channel_id, contact_id)
VALUES(:org_id, :identity, :path, :scheme, :display, :priority, :channel_id, :contact_id)
RETURNING id
`

// InsertContactURN inserts the passed in urn, the id field will be populated with the result on success
func insertContactURN(db *sqlx.DB, urn *DBContactURN) error {
	rows, err := db.NamedQuery(insertURN, urn)
	if err != nil {
		return err
	}
	defer rows.Close()

	if rows.Next() {
		err = rows.Scan(&urn.ID)
	}
	return err
}

const updateURN = `
UPDATE contacts_contacturn
SET channel_id = :channel_id, contact_id = :contact_id, display = :display, priority = :priority
WHERE id = :id
`

// UpdateContactURN updates the Channel and Contact on an existing URN
func updateContactURN(db *sqlx.DB, urn *DBContactURN) error {
	rows, err := db.NamedQuery(updateURN, urn)
	if err != nil {
		return err
	}
	defer rows.Close()

	if rows.Next() {
		rows.Scan(&urn.ID)
	}
	return err
}

// DBContactURN is our struct to map to database level URNs
type DBContactURN struct {
	OrgID     OrgID             `db:"org_id"`
	ID        ContactURNID      `db:"id"`
	Identity  string            `db:"identity"`
	Scheme    string            `db:"scheme"`
	Path      string            `db:"path"`
	Display   null.String       `db:"display"`
	Priority  int               `db:"priority"`
	ChannelID courier.ChannelID `db:"channel_id"`
	ContactID ContactID         `db:"contact_id"`
}
