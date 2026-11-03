// Package coretest provides in-memory implementations of every interface in
// package core, for unit tests that must not need PostgreSQL, S3 or NATS.
//
// The fakes follow the documented contracts closely enough that a format
// handler tested against them behaves the same against the real services:
// write policies (allow, allow_once, deny), ErrNotFound / ErrWriteDenied /
// ErrForbidden semantics, events and audit records on writes, anonymous read.
//
// Typical use in a format package:
//
//	env := coretest.NewEnv(t)
//	repo := env.AddRepo(t, &core.Repository{Name: "raw-hosted", Format: "raw", Type: core.RepoHosted})
//	env.Auth.AddUser("alice", "secret")
//	env.AuthZ.Grant("alice", "repo:raw-hosted:write")
//	h := formats.NewHandler(env.Deps(), raw.New())   // routes mounted exactly like in production
//	// drive h with httptest
//
// RunBlobStoreTests is a conformance suite every BlobStore driver must pass.
package coretest
