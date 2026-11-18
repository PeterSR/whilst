// Package boundary holds only a test: it checks that the core stays free of
// the platform. It lives in a package of its own because it runs go list,
// and so is the one package in the core allowed to import os/exec.
package boundary
