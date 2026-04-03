// Package idmap provides deterministic ObjectId → UUID conversion using UUID v5.
// The same ObjectId will always produce the same UUID, making the mapping
// reproducible across re-runs without storing a lookup table.
package idmap

import (
	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// migrationNamespace is a fixed UUID v5 namespace specific to this migration.
// Do NOT change after the first run.
var migrationNamespace = uuid.MustParse("a1b2c3d4-e5f6-7890-abcd-ef1234567890")

// FromObjectID converts a MongoDB ObjectId to a deterministic UUID.
func FromObjectID(oid bson.ObjectID) uuid.UUID {
	return uuid.NewSHA1(migrationNamespace, []byte(oid.Hex()))
}

// FromString converts a MongoDB ObjectId hex string to a deterministic UUID.
// Returns uuid.Nil if the string is not a valid ObjectId.
func FromString(hexStr string) uuid.UUID {
	oid, err := bson.ObjectIDFromHex(hexStr)
	if err != nil {
		return uuid.Nil
	}
	return FromObjectID(oid)
}

// MustFromString panics if the hex string is not a valid ObjectId.
func MustFromString(hexStr string) uuid.UUID {
	u := FromString(hexStr)
	if u == uuid.Nil {
		panic("invalid ObjectId hex: " + hexStr)
	}
	return u
}
