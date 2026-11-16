package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// The MMHotline contact card (/mmhotline-contact-card.vcf) holds every
// calling number, plus the extra phone numbers from the Twilio setup, so a
// person can let all of them through Do Not Disturb. A call from a
// number not on the card would be silenced on a sleeping phone. When the
// card's numbers change, every member with a automated voice call contact method is
// told, and the contact methods page shows a notice until they download it
// again.

// ContactCardNumbers lists the card's numbers, sorted, each once.
func (p *TwilioSendingService) ContactCardNumbers() []string {
	var numbers []string
	if t := p.Twilio; t != nil {
		numbers = append(numbers, t.PhoneNumberToCallFrom)
	}
	numbers = append(numbers, p.ExtraPhoneNumbersOnTheContactCard...)
	numbers = slices.DeleteFunc(numbers, func(n string) bool { return !strings.HasPrefix(n, "+") })
	slices.Sort(numbers)
	return slices.Compact(numbers)
}

// contactCardFingerprint names one version of the card's numbers.
func contactCardFingerprint(numbers []string) string {
	sum := sha256.Sum256([]byte(strings.Join(numbers, ",")))
	return hex.EncodeToString(sum[:8])
}

// contactCardVCF is the card in vCard 3.0 (RFC 2426).
func contactCardVCF(numbers []string) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:MMHotline\r\nN:;MMHotline;;;\r\nORG:MMHotline\r\n")
	for _, n := range numbers {
		b.WriteString("TEL;TYPE=VOICE:" + n + "\r\n")
	}
	b.WriteString("NOTE:MMHotline calls from these numbers when an SOS incident needs you. Allow this contact through Do Not Disturb.\r\nEND:VCARD\r\n")
	return b.String()
}

// ContactCardState is what the contact methods page says about the card for
// one member.
type ContactCardState struct {
	// Available is false when no phone sending service is set up.
	Available bool
	// Changed is true when the member downloaded an earlier version.
	Changed bool
	// NeverDownloaded is true when the member has not downloaded it yet.
	NeverDownloaded bool
}

func (a *App) contactCardState(ctx context.Context, tx *sql.Tx, memberID string) (ContactCardState, error) {
	sendingServices := a.twilioSendingService()
	if sendingServices == nil {
		return ContactCardState{}, nil
	}
	state := ContactCardState{Available: true}
	var downloaded string
	err := tx.QueryRowContext(ctx, `SELECT contact_card_fingerprint FROM contact_card_downloads WHERE member_id=?`, memberID).Scan(&downloaded)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		state.NeverDownloaded = true
	case err != nil:
		return state, err
	default:
		state.Changed = downloaded != contactCardFingerprint(sendingServices.ContactCardNumbers())
	}
	return state, nil
}

func (a *App) contactCardHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pagePrincipal(w, r)
	if !ok {
		return
	}
	sendingServices := a.twilioSendingService()
	if sendingServices == nil {
		HTTPError(w, ErrNotFound)
		return
	}
	numbers := sendingServices.ContactCardNumbers()
	err := a.Store.WithTransaction(r.Context(), func(tx *sql.Tx) error {
		m, err := RequireMember(r.Context(), tx, p, false)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(r.Context(), `INSERT INTO contact_card_downloads(member_id,contact_card_fingerprint,downloaded_at) VALUES(?,?,?) ON CONFLICT(member_id) DO UPDATE SET contact_card_fingerprint=excluded.contact_card_fingerprint,downloaded_at=excluded.downloaded_at`, m.ID, contactCardFingerprint(numbers), a.Now().Unix())
		return err
	})
	if err != nil {
		HTTPError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="mmhotline-contact-card.vcf"`)
	fmt.Fprint(w, contactCardVCF(numbers))
}

// AnnounceContactCardChange tells every member with a automated voice call contact
// method, once, when the card's numbers change. The first version a team
// sees is not a change.
func (a *App) AnnounceContactCardChange(ctx context.Context) error {
	sendingServices := a.twilioSendingService()
	if sendingServices == nil {
		return nil
	}
	current := contactCardFingerprint(sendingServices.ContactCardNumbers())
	now := a.Now()
	return a.Store.WithTransaction(ctx, func(tx *sql.Tx) error {
		var announced string
		err := tx.QueryRowContext(ctx, `SELECT contact_card_fingerprint FROM contact_card_announcements WHERE id=1`).Scan(&announced)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, `INSERT INTO contact_card_announcements(id,contact_card_fingerprint) VALUES(1,?)`, current)
			return err
		}
		if err != nil || announced == current {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE contact_card_announcements SET contact_card_fingerprint=? WHERE id=1`, current); err != nil {
			return err
		}
		kinds := contactMethodTypesWith(func(f ContactMethodTypeFacts) bool { return f.ValueFixed })
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT member_id FROM contact_methods WHERE deleted=0 AND member_id!='' AND contact_method_type IN (`+sqlPlaceholders(len(kinds))+`)`, kinds...)
		if err != nil {
			return err
		}
		var members []string
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			members = append(members, id)
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil || len(members) == 0 {
			return err
		}
		link := strings.TrimRight(a.WebURL, "/") + "/contact-methods"
		_, err = admitAlert(ctx, tx, "system", "system", "local:"+NewID(), "", GenericAlert{Severity: "info", Title: "MMHotline calls from new numbers: download its contact card again", Message: "MMHotline now calls from new numbers. Download the MMHotline contact card again and allow the MMHotline contact through Do Not Disturb, so SOS calls still wake you: " + link}, members, now)
		return err
	})
}
