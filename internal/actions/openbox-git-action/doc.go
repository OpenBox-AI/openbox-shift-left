// Package gitaction is the OpenBox git action: the server-side, push-time
// resolver that binds a pushed commit to the OpenBox session(s) that produced
// it and registers a Deploy governance event provably linked to those
// sessions.
//   - Attributed; >=1 session verified as owned by the authenticated pusher. -
package gitaction
