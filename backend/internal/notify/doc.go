// Package notify turns notification rows into what a person reads, in the app and by email.
//
// A notification row says what happened and to which record, never the text: Renderer fills in its template from the
// record as the recipient may see it (their RLS), so neither the bell nor an email ever says more than they could read
// for themselves. Email goes out from the worker (Mailer.Handler, job kind notify.email): a trigger queues one delivery
// per notification that asks for email (migration 00029), and the handler renders it as the recipient and sends it
// through the mail server in the email settings, with the password decrypted only for that.
package notify
