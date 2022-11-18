package blobs

import (
	"math/big"
	"math/bits"

	"github.com/ethereum/go-ethereum/crypto/kzg"
	"github.com/ethereum/go-ethereum/params"
	"github.com/pkg/errors"
	"github.com/protolambda/go-kzg/bls"
	ssz "github.com/prysmaticlabs/fastssz"
	types "github.com/prysmaticlabs/prysm/v3/consensus-types/primitives"
	"github.com/prysmaticlabs/prysm/v3/crypto/hash"
	"github.com/prysmaticlabs/prysm/v3/encoding/bytesutil"
	v1 "github.com/prysmaticlabs/prysm/v3/proto/engine/v1"
	eth "github.com/prysmaticlabs/prysm/v3/proto/prysm/v1alpha1"
)

var (
	ErrInvalidBlobSlot            = errors.New("invalid blob slot")
	ErrInvalidBlobBeaconBlockRoot = errors.New("invalid blob beacon block root")
	ErrInvalidBlobsLength         = errors.New("invalid blobs length")
	ErrEmptyBlobsInSidecar        = errors.New("no blobs found in sidecar")
	ErrInvalidAggregateProof      = errors.New("couldn't verify proof")

	blsModulus   big.Int
	rootsOfUnity []bls.Fr
)

func init() {
	blsModulus.SetString(bls.ModulusStr, 10)

	// Initialize rootsOfUnity which are used by EvaluatePolyInEvaluationForm
	var one big.Int
	one.SetInt64(1)
	var length big.Int
	length.SetInt64(params.FieldElementsPerBlob)

	var divisor big.Int
	divisor.Sub(&blsModulus, &one)
	if new(big.Int).Mod(&divisor, &length).Int64() != 0 {
		panic("MODULUS-1 % FieldElementsPerBlob should equal 0")
	}
	divisor.Div(&divisor, &length) // divisor == MODULUS - 1 / length

	var rootOfUnity big.Int
	rootOfUnity.SetInt64(7) // PRIMITIVE_ROOT_OF_UNITY
	rootOfUnity.Exp(&rootOfUnity, &divisor, &blsModulus)

	current := one
	rootsOfUnity = make([]bls.Fr, params.FieldElementsPerBlob)
	for i := 0; i < params.FieldElementsPerBlob; i++ {
		bigToFr(&rootsOfUnity[i], &current)
		current.Mul(&current, &rootOfUnity).
			Mod(&current, &blsModulus)
	}

	rootsOfUnity = bitReversalPermutation(rootsOfUnity)
}

// ValidateBlobsSidecar validates the integrity of a sidecar.
//
// def validate_blobs_sidecar(slot: Slot,
//
//	                       beacon_block_root: Root,
//	                       expected_kzg_commitments: Sequence[KZGCommitment],
//	                       blobs_sidecar: BlobsSidecar) -> None:
//	assert slot == blobs_sidecar.beacon_block_slot
//	assert beacon_block_root == blobs_sidecar.beacon_block_root
//	blobs = blobs_sidecar.blobs
//	kzg_aggregated_proof = blobs_sidecar.kzg_aggregated_proof
//	assert len(expected_kzg_commitments) == len(blobs)
//
//	assert verify_aggregate_kzg_proof(blobs, expected_kzg_commitments, kzg_aggregated_proof)
func ValidateBlobsSidecar(slot types.Slot, root [32]byte, commitments [][]byte, sidecar *eth.BlobsSidecar) error {
	if slot != sidecar.BeaconBlockSlot {
		return ErrInvalidBlobSlot
	}
	if root != bytesutil.ToBytes32(sidecar.BeaconBlockRoot) {
		return ErrInvalidBlobBeaconBlockRoot
	}
	if len(commitments) != len(sidecar.Blobs) {
		return ErrInvalidBlobsLength
	}

	rData := &eth.BlobsAndCommitments{
		Blobs:       sidecar.Blobs,
		Commitments: commitments,
	}

	r, err := hashToBlsField(rData)
	if err != nil {
		return err
	}

	numberOfBlobs := len(sidecar.Blobs)
	if numberOfBlobs == 0 {
		return ErrEmptyBlobsInSidecar
	}
	rPowers := computePowers(r, numberOfBlobs)

	aggregatedPolyCommitment, err := g1LinComb(commitments, rPowers)
	if err != nil {
		return err
	}

	aggregatedPoly, err := polyLinComb(sidecar.Blobs, rPowers)
	if err != nil {
		return err
	}

	xData := &eth.PolynomialAndCommitment{
		Polynomial: make([][]byte, params.FieldElementsPerBlob),
		Commitment: aggregatedPolyCommitment,
	}
	for i := 0; i < params.FieldElementsPerBlob; i++ {
		v := bls.FrTo32(&aggregatedPoly[i])
		xData.Polynomial[i] = v[:]
	}

	x, err := hashToBlsField(xData)
	if err != nil {
		return err
	}

	var y bls.Fr
	bls.EvaluatePolyInEvaluationForm(&y, aggregatedPoly, x, rootsOfUnity, 0)

	b, err := kzg.VerifyKZGProof(bytesutil.ToBytes48(aggregatedPolyCommitment), x, &y, bytesutil.ToBytes48(sidecar.AggregatedProof))
	if err != nil {
		return err
	}
	if !b {
		return ErrInvalidAggregateProof
	}
	return nil
}

// hashToBlsField computes the 32-byte hash of serialized container and convert it to BLS field.
// The output is not uniform over the BLS field.
//
// Spec code:
// def hash_to_bls_field(x: Container) -> BLSFieldElement:
//
//	return bytes_to_bls_field(hash(ssz_serialize(x)))
func hashToBlsField(x ssz.Marshaler) (*bls.Fr, error) {
	m, err := x.MarshalSSZ()
	if err != nil {
		return nil, err
	}

	h := hash.Hash(m)

	var b big.Int
	// Reverse the byte to interpret hash `h` as a little-endian integer then
	// mod it with the BLS modulus.
	b.SetBytes(bytesutil.ReverseByteOrder(h[:])).Mod(&b, &blsModulus)

	// Convert big int from above to field element.
	var f *bls.Fr
	bls.SetFr(f, b.String())
	return f, nil
}

// computePowers returns the power of field element input `x` to power of [0, n-1].
//
// spec code:
// def compute_powers(x: BLSFieldElement, n: uint64) -> Sequence[BLSFieldElement]:
//
//	current_power = 1
//	powers = []
//	for _ in range(n):
//	    powers.append(BLSFieldElement(current_power))
//	    current_power = current_power * int(x) % BLS_MODULUS
//	return powers
func computePowers(x *bls.Fr, n int) []bls.Fr {
	currentPower := bls.ONE
	powers := make([]bls.Fr, n)
	for i := range powers {
		powers[i] = currentPower
		bls.MulModFr(&currentPower, &currentPower, x)
	}
	return powers
}

// polyLinComb interpret the input `blobs` as a 2D matrix and compute the linear combination with `scalars`.
//
// spce code:
// def poly_lincomb(polys: Sequence[Polynomial],
//
//	             scalars: Sequence[BLSFieldElement]) -> Polynomial:
//	"""
//	Given a list of ``polynomials``, interpret it as a 2D matrix and compute the linear combination
//	of each column with `scalars`: return the resulting polynomials.
//	"""
//	result = [0] * len(polys[0])
//	for v, s in zip(polys, scalars):
//	    for i, x in enumerate(v):
//	        result[i] = (result[i] + int(s) * int(x)) % BLS_MODULUS
//	return [BLSFieldElement(x) for x in result]
func polyLinComb(blobs []*v1.Blob, scalars []bls.Fr) ([]bls.Fr, error) {
	out := make([][]bls.Fr, len(blobs))
	for i := range blobs {
		r := make([]bls.Fr, params.FieldElementsPerBlob)
		blob := blobs[i].Data
		for j := 0; j <= params.FieldElementsPerBlob; j++ {
			b := blob[j*32 : j*32+31]
			ok := bls.FrFrom32(&r[j], bytesutil.ToBytes32(b))
			if !ok {
				return nil, errors.New("invalid value in blob")
			}
		}
		out[i] = r
	}
	return bls.PolyLinComb(out, scalars)
}

// g1LinComb performs BLS multi-scalar multiplication for input `points` to `scalars`.
//
// spec code:
// def g1_lincomb(points: Sequence[KZGCommitment], scalars: Sequence[BLSFieldElement]) -> KZGCommitment:
//
//	"""
//	BLS multiscalar multiplication. This function can be optimized using Pippenger's algorithm and variants.
//	"""
//	assert len(points) == len(scalars)
//	result = bls.Z1
//	for x, a in zip(points, scalars):
//	    result = bls.add(result, bls.multiply(bls.bytes48_to_G1(x), a))
//	return KZGCommitment(bls.G1_to_bytes48(result))
func g1LinComb(points [][]byte, scalars []bls.Fr) ([]byte, error) {
	if len(points) != len(scalars) {
		return nil, errors.New("points and scalars have to be the same length")
	}
	g1s := make([]bls.G1Point, len(points))
	for i := range g1s {
		g1, err := bls.FromCompressedG1(points[i])
		if err != nil {
			return nil, err
		}
		g1s[i] = *g1
	}
	return bls.ToCompressedG1(bls.LinCombG1(g1s, scalars)), nil
}

// bigToFr converts the big.Int represented BLS field element b to the go-kzg library
// representation bls.Fr, putting its value in fr and returning it.
func bigToFr(fr *bls.Fr, b *big.Int) *bls.Fr {
	// TODO: Conversion currently relies the string representation as an intermediary.  Submit a PR
	// to protolambda/go-kzg enabling something more efficient.
	bls.SetFr(fr, b.String())
	return fr
}

// Return a copy with bit-reversed permutation. This operation is idempotent.
// l is the array of roots of unity
func bitReversalPermutation(l []bls.Fr) []bls.Fr {
	if !isPowerOfTwo(params.FieldElementsPerBlob) {
		panic("params.FieldElementsPerBlob must be a power of two")
	}

	out := make([]bls.Fr, params.FieldElementsPerBlob)
	for i := range l {
		j := bits.Reverse64(uint64(i)) >> (65 - bits.Len64(params.FieldElementsPerBlob))
		out[i] = l[j]
	}

	return out
}

func isPowerOfTwo(value uint64) bool {
	return value > 0 && (value&(value-1) == 0)
}