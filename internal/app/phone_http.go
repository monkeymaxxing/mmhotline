package app

import (
	"net/http"
)

// RegisterPhoneRoutes adds phone number forms and the contact card.
func (a *App) RegisterPhoneRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /contact-methods/phone-number", a.addPhoneNumberHTTP)
	mux.HandleFunc("GET /mmhotline-contact-card.vcf", a.contactCardHTTP)
}

func (a *App) addPhoneNumberHTTP(w http.ResponseWriter, r *http.Request) {
	p, ok := a.contactMethodMutation(w, r)
	if !ok {
		return
	}
	typed := r.PostForm.Get("phone_number_to_call")
	added, err := a.AddPhoneNumber(r.Context(), p, typed)
	if err != nil {
		a.renderContactMethodsWithPhoneNumber(w, r, p, errorStatus(err), publicError(err), "", typed)
		return
	}
	notice := "Phone number " + added.PhoneNumberToCall + " added. A test call is queued."
	if reason := added.ContactMethods[0].TestNotSentReason; reason != "" {
		notice = "Phone number " + added.PhoneNumberToCall + " added. " + reason
	}
	a.renderContactMethodsWithPhoneNumber(w, r, p, 200, "", notice, "")
}
