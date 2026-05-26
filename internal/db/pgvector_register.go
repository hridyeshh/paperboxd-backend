package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// RegisterPgvectorTypes wires pgvector codecs into a pgx connection, mirroring
// pgxvec.RegisterTypes but wrapping the vector codec so NULL column values do
// not panic on scan.
//
// Upstream bug: pgvector-go/pgx@v0.4.0 scanPlanVectorCodecBinary.Scan calls
// DecodeBinary(nil) when a row's vector column is NULL, which slice-indexes
// the empty buffer and panics with
// "slice bounds out of range [:2] with capacity 0".
//
// We register the same OIDs but with a wrapper codec whose ScanPlan resets the
// destination Vector to its zero value when src is nil (i.e. NULL), and
// delegates to the upstream plan otherwise. halfvec / sparsevec we don't write
// in this schema, so we leave them on the upstream codecs.
func RegisterPgvectorTypes(ctx context.Context, conn *pgx.Conn) error {
	var vectorOid *uint32
	var vectorArrayOid *uint32
	var halfvecOid *uint32
	var halfvecArrayOid *uint32
	var sparsevecOid *uint32
	var sparsevecArrayOid *uint32
	err := conn.QueryRow(
		ctx,
		"SELECT to_regtype('vector')::oid, to_regtype('_vector')::oid, "+
			"to_regtype('halfvec')::oid, to_regtype('_halfvec')::oid, "+
			"to_regtype('sparsevec')::oid, to_regtype('_sparsevec')::oid",
	).Scan(&vectorOid, &vectorArrayOid, &halfvecOid, &halfvecArrayOid, &sparsevecOid, &sparsevecArrayOid)
	if err != nil {
		return fmt.Errorf("query pgvector oids: %w", err)
	}
	if vectorOid == nil {
		return fmt.Errorf("vector type not found in the database")
	}

	tm := conn.TypeMap()

	vecType := pgtype.Type{Name: "vector", OID: *vectorOid, Codec: &nullSafeVectorCodec{}}
	tm.RegisterType(&vecType)
	if vectorArrayOid != nil {
		tm.RegisterType(&pgtype.Type{Name: "_vector", OID: *vectorArrayOid, Codec: &pgtype.ArrayCodec{ElementType: &vecType}})
	}

	// halfvec / sparsevec: use upstream codecs unchanged. If we ever start
	// storing them as nullable too, wrap them the same way.
	if halfvecOid != nil {
		hvType := pgtype.Type{Name: "halfvec", OID: *halfvecOid, Codec: &pgxvec.HalfVectorCodec{}}
		tm.RegisterType(&hvType)
		if halfvecArrayOid != nil {
			tm.RegisterType(&pgtype.Type{Name: "_halfvec", OID: *halfvecArrayOid, Codec: &pgtype.ArrayCodec{ElementType: &hvType}})
		}
	}
	if sparsevecOid != nil {
		svType := pgtype.Type{Name: "sparsevec", OID: *sparsevecOid, Codec: &pgxvec.SparseVectorCodec{}}
		tm.RegisterType(&svType)
		if sparsevecArrayOid != nil {
			tm.RegisterType(&pgtype.Type{Name: "_sparsevec", OID: *sparsevecArrayOid, Codec: &pgtype.ArrayCodec{ElementType: &svType}})
		}
	}

	return nil
}

// nullSafeVectorCodec embeds the upstream VectorCodec and overrides PlanScan
// so NULL column values resolve to a zero-value pgvector.Vector instead of
// panicking inside DecodeBinary.
type nullSafeVectorCodec struct {
	pgxvec.VectorCodec
}

func (c *nullSafeVectorCodec) PlanScan(m *pgtype.Map, oid uint32, format int16, target any) pgtype.ScanPlan {
	if _, ok := target.(*pgvector.Vector); !ok {
		return nil
	}
	inner := c.VectorCodec.PlanScan(m, oid, format, target)
	if inner == nil {
		return nil
	}
	return nullSafeVectorScanPlan{inner: inner}
}

type nullSafeVectorScanPlan struct {
	inner pgtype.ScanPlan
}

func (p nullSafeVectorScanPlan) Scan(src []byte, dst any) error {
	if src == nil {
		if v, ok := dst.(*pgvector.Vector); ok {
			*v = pgvector.Vector{}
		}
		return nil
	}
	return p.inner.Scan(src, dst)
}
