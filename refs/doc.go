// Package refs supplies optional, single-valued cross-domain reference maps
// and versioned resource definitions. It is independent of persistence drivers
// and identity context: a resource may explicitly include Values, while an
// application shares a Schema contract with its producers and consumers.
//
// Values has a strict flat JSON object representation and detached snapshots.
// Schema validates allowed keys, ID spelling, required roles and write-once
// roles. Its validation does not verify target existence, ownership, tenant
// membership or authorization; those checks belong to explicit domain contracts.
// Refs are never implicitly propagated through callctx or used as identity.
//
// The package provides neither an ORM nor SQL query/index generation. A domain
// can persist the JSON representation in its own adapter or project hot roles
// into native columns. Schema versions and any migrations remain explicit
// resource-contract decisions; no global registry is installed.
package refs
