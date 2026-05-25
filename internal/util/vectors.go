package util

import "math"

// CosineSimilarity returns the cosine similarity between two float32 vectors.
// Returns 0 if vectors are empty or have different lengths.
func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// ComputeCentroid returns the element-wise mean of a slice of vectors.
// Returns nil if vecs is empty. All vectors must have the same dimension.
func ComputeCentroid(vecs [][]float32) []float32 {
	if len(vecs) == 0 {
		return nil
	}
	dims := len(vecs[0])
	centroid := make([]float32, dims)
	for _, v := range vecs {
		for i, val := range v {
			centroid[i] += val
		}
	}
	n := float32(len(vecs))
	for i := range centroid {
		centroid[i] /= n
	}
	return centroid
}

// Min32 returns the smaller of two float32 values.
func Min32(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}
