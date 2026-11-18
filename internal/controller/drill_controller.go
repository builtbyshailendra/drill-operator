// Package controller reconciles Drills.
package controller

// AllowLabel opts a namespace in to drills. Without it the operator refuses
// to touch anything there.
const AllowLabel = "drills.builtbyshailendra.dev/allowed"
