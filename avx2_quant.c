#include <immintrin.h>
#include <stdint.h>

/*
 * cpu_simd_level reports the widest vector path the CPU supports.
 * 2 means AVX2, 3 means AVX-512, anything else falls back to scalar.
 */
int cpu_simd_level(void) {
    static int level = -1;
    if (level < 0) {
        level = 2;
        if (__builtin_cpu_supports("avx512f") &&
            __builtin_cpu_supports("avx512bw") &&
            __builtin_cpu_supports("avx512dq") &&
            __builtin_cpu_supports("avx512vl") &&
            __builtin_cpu_supports("avx512vbmi")) {
            level = 3;
        }
    }
    return level;
}

/*
 * vert_acc_init_avx2 seeds an accumulator row with one weighted source
 * row, replacing the separate zero fill and first accumulate.
 */
__attribute__((target("avx2"))) static void vert_acc_init_avx2(uint32_t *acc, const unsigned char *row, uint32_t w, int n) {
    const __m256i vw = _mm256_set1_epi32((int)w);
    int i = 0;
    for (; i + 8 <= n; i += 8) {
        __m128i v8 = _mm_loadl_epi64((const __m128i *)(row + i));
        __m256i v = _mm256_cvtepu8_epi32(v8);
        _mm256_storeu_si256((__m256i *)(acc + i), _mm256_mullo_epi32(v, vw));
    }
    for (; i < n; i++) {
        acc[i] = (uint32_t)row[i] * w;
    }
}

/*
 * vert_acc_avx2 adds one weighted source row into an accumulator row.
 * The arithmetic is exact 32 bit, matching the scalar reference.
 */
__attribute__((target("avx2"))) static void vert_acc_avx2(uint32_t *acc, const unsigned char *row, uint32_t w, int n) {
    const __m256i vw = _mm256_set1_epi32((int)w);
    int i = 0;
    for (; i + 8 <= n; i += 8) {
        __m128i v8 = _mm_loadl_epi64((const __m128i *)(row + i));
        __m256i v = _mm256_cvtepu8_epi32(v8);
        __m256i a = _mm256_loadu_si256((const __m256i *)(acc + i));
        _mm256_storeu_si256((__m256i *)(acc + i), _mm256_add_epi32(a, _mm256_mullo_epi32(v, vw)));
    }
    for (; i < n; i++) {
        acc[i] += (uint32_t)row[i] * w;
    }
}

/*
 * norm_acc_avx2 normalizes accumulator rows to bytes with the exact
 * rounding of (acc * inv + 2^31) >> 32, computed without table lookups.
 */
__attribute__((target("avx2"))) static void norm_acc_avx2(unsigned char *dst, const unsigned int *acc, unsigned int inv, int n) {
    const __m256i vinv = _mm256_set1_epi32((int)inv);
    int i = 0;
    for (; i + 8 <= n; i += 8) {
        __m256i a = _mm256_loadu_si256((const __m256i *)(acc + i));
        __m256i pe = _mm256_mul_epu32(a, vinv);
        __m256i po = _mm256_mul_epu32(_mm256_srli_epi64(a, 32), _mm256_srli_epi64(vinv, 32));
        __m256i hi = _mm256_or_si256(_mm256_srli_epi64(pe, 32), _mm256_slli_epi64(_mm256_srli_epi64(po, 32), 32));
        __m256i lo = _mm256_mullo_epi32(a, vinv);
        __m256i q = _mm256_add_epi32(hi, _mm256_srli_epi32(lo, 31));
        __m128i p16 = _mm_packus_epi32(_mm256_castsi256_si128(q), _mm256_extracti128_si256(q, 1));
        __m128i p8 = _mm_packus_epi16(p16, p16);
        _mm_storel_epi64((__m128i *)(dst + i), p8);
    }
    for (; i < n; i++) {
        dst[i] = (unsigned char)(((unsigned long long)acc[i] * inv + (1ULL << 31)) >> 32);
    }
}

/*
 * rgba_matrix_avx2 converts one normalized YUV row to RGBA with the
 * BT.601 limited range matrix, clamped exactly like the scalar code.
 */
__attribute__((target("avx2"))) static void rgba_matrix_avx2(unsigned char *dst, const unsigned char *ty,
                      const unsigned char *tu, const unsigned char *tv, int n) {
    const __m256i vy = _mm256_set1_epi32(16);
    const __m256i vc = _mm256_set1_epi32(128);
    const __m256i k298 = _mm256_set1_epi32(298);
    const __m256i k409 = _mm256_set1_epi32(409);
    const __m256i k100 = _mm256_set1_epi32(100);
    const __m256i k208 = _mm256_set1_epi32(208);
    const __m256i k516 = _mm256_set1_epi32(516);
    const __m256i vrnd = _mm256_set1_epi32(128);
    const __m256i vzero = _mm256_setzero_si256();
    const __m256i v255 = _mm256_set1_epi32(255);
    const __m256i valpha = _mm256_set1_epi32((int)0xFF000000u);
    int i = 0;
    for (; i + 8 <= n; i += 8) {
        __m128i y8 = _mm_loadl_epi64((const __m128i *)(ty + i));
        __m128i u8v = _mm_loadl_epi64((const __m128i *)(tu + i));
        __m128i v8 = _mm_loadl_epi64((const __m128i *)(tv + i));
        __m256i c298 = _mm256_mullo_epi32(_mm256_sub_epi32(_mm256_cvtepu8_epi32(y8), vy), k298);
        __m256i dd = _mm256_sub_epi32(_mm256_cvtepu8_epi32(u8v), vc);
        __m256i ee = _mm256_sub_epi32(_mm256_cvtepu8_epi32(v8), vc);
        __m256i r = _mm256_srai_epi32(_mm256_add_epi32(_mm256_add_epi32(c298, _mm256_mullo_epi32(ee, k409)), vrnd), 8);
        __m256i g = _mm256_srai_epi32(_mm256_add_epi32(_mm256_sub_epi32(_mm256_sub_epi32(c298, _mm256_mullo_epi32(dd, k100)), _mm256_mullo_epi32(ee, k208)), vrnd), 8);
        __m256i b = _mm256_srai_epi32(_mm256_add_epi32(_mm256_add_epi32(c298, _mm256_mullo_epi32(dd, k516)), vrnd), 8);
        r = _mm256_min_epi32(_mm256_max_epi32(r, vzero), v255);
        g = _mm256_min_epi32(_mm256_max_epi32(g, vzero), v255);
        b = _mm256_min_epi32(_mm256_max_epi32(b, vzero), v255);
        __m256i px = _mm256_or_si256(_mm256_or_si256(r, _mm256_slli_epi32(g, 8)),
                                     _mm256_or_si256(_mm256_slli_epi32(b, 16), valpha));
        _mm256_storeu_si256((__m256i *)(dst + i * 4), px);
    }
    for (; i < n; i++) {
        int cc = (int)ty[i] - 16;
        int dd = (int)tu[i] - 128;
        int ee = (int)tv[i] - 128;
        int r = (298 * cc + 409 * ee + 128) >> 8;
        int g = (298 * cc - 100 * dd - 208 * ee + 128) >> 8;
        int b = (298 * cc + 516 * dd + 128) >> 8;
        if (r < 0) r = 0; else if (r > 255) r = 255;
        if (g < 0) g = 0; else if (g > 255) g = 255;
        if (b < 0) b = 0; else if (b > 255) b = 255;
        dst[i * 4 + 0] = (unsigned char)r;
        dst[i * 4 + 1] = (unsigned char)g;
        dst[i * 4 + 2] = (unsigned char)b;
        dst[i * 4 + 3] = 255;
    }
}

/*
 * hash_strip_avx2 hashes one sixel band of RGBA pixels with a Horner
 * style polynomial over eight 32-bit lanes and two multiplier sets.
 * The result matches the scalar reference in sixel.odin exactly.
 */
uint64_t hash_strip_avx2(const unsigned char *rgba, int width, int y_start, int y_end) {
    const uint32_t P1 = 0x9E3779B1u, P2 = 0x85EBCA77u;
    const uint32_t P3 = 0xC2B2AE3Du, P4 = 0x27D4EB2Fu;
    const unsigned char *p = rgba + (size_t)y_start * width * 4;
    const size_t nwords = (size_t)(y_end - y_start) * width;
    const size_t nvec = nwords / 8 * 8;

    const __m256i vp1 = _mm256_set1_epi32((int)P1);
    const __m256i vp2 = _mm256_set1_epi32((int)P2);
    const __m256i vp3 = _mm256_set1_epi32((int)P3);
    const __m256i vp4 = _mm256_set1_epi32((int)P4);
    __m256i acc1 = _mm256_set1_epi32((int)0x9E3779B9u);
    __m256i acc2 = _mm256_set1_epi32((int)0x85EBCA6Bu);

    for (size_t i = 0; i < nvec; i += 8) {
        __m256i v = _mm256_loadu_si256((const __m256i *)(p + i * 4));
        acc1 = _mm256_add_epi32(_mm256_mullo_epi32(acc1, vp1), _mm256_mullo_epi32(v, vp2));
        acc2 = _mm256_add_epi32(_mm256_mullo_epi32(acc2, vp3), _mm256_mullo_epi32(v, vp4));
    }

    uint32_t a1[8], a2[8];
    _mm256_storeu_si256((__m256i *)a1, acc1);
    _mm256_storeu_si256((__m256i *)a2, acc2);

    for (size_t i = nvec; i < nwords; i++) {
        const unsigned char *q = p + i * 4;
        uint32_t v = (uint32_t)q[0] | ((uint32_t)q[1] << 8) |
                     ((uint32_t)q[2] << 16) | ((uint32_t)q[3] << 24);
        const size_t k = i % 8;
        a1[k] = a1[k] * P1 + v * P2;
        a2[k] = a2[k] * P3 + v * P4;
    }

    uint32_t s1 = 0, s2 = 0;
    for (int k = 0; k < 8; k++) {
        s1 += a1[k] * (uint32_t)(2 * k + 1);
        s2 += a2[k] * (uint32_t)(2 * k + 1);
    }
    return ((uint64_t)s1 << 32) | (uint64_t)s2;
}

/*
 * quant_row_avx2 quantizes 8 RGBA pixels per iteration using 256-bit
 * registers. Each channel has its own scaled Bayer dither pair in lane
 * order: bytes 0..3 for pixels 0..3 and bytes 16..19 for pixels 4..7.
 */
__attribute__((target("avx2"))) static void quant_row_avx2(unsigned char *dst, const unsigned char *rgba, int w,
                    const unsigned char *dpos_r, const unsigned char *dneg_r,
                    const unsigned char *dpos_g, const unsigned char *dneg_g,
                    const unsigned char *dpos_b, const unsigned char *dneg_b,
                    int r_shr, int g_shr, int b_shr,
                    int r_shift, int g_shift) {
    const __m128i m0 = _mm_setr_epi8(0, 4, 8, 12, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1);
    __m256i mask_r = _mm256_set_m128i(m0, m0);
    const __m128i m1 = _mm_setr_epi8(1, 5, 9, 13, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1);
    __m256i mask_g = _mm256_set_m128i(m1, m1);
    const __m128i m2 = _mm_setr_epi8(2, 6, 10, 14, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1, -1);
    __m256i mask_b = _mm256_set_m128i(m2, m2);

    __m256i dpr = _mm256_loadu_si256((const __m256i *)dpos_r);
    __m256i dnr = _mm256_loadu_si256((const __m256i *)dneg_r);
    __m256i dpg = _mm256_loadu_si256((const __m256i *)dpos_g);
    __m256i dng = _mm256_loadu_si256((const __m256i *)dneg_g);
    __m256i dpb = _mm256_loadu_si256((const __m256i *)dpos_b);
    __m256i dnb = _mm256_loadu_si256((const __m256i *)dneg_b);
    __m256i zero = _mm256_setzero_si256();

    for (int x = 0; x + 8 <= w; x += 8) {
        __m256i px = _mm256_loadu_si256((const __m256i *)(rgba + x * 4));

        __m256i rb = _mm256_shuffle_epi8(px, mask_r);
        __m256i gb = _mm256_shuffle_epi8(px, mask_g);
        __m256i bb = _mm256_shuffle_epi8(px, mask_b);

        rb = _mm256_subs_epu8(_mm256_adds_epu8(rb, dpr), dnr);
        gb = _mm256_subs_epu8(_mm256_adds_epu8(gb, dpg), dng);
        bb = _mm256_subs_epu8(_mm256_adds_epu8(bb, dpb), dnb);

        __m256i r16 = _mm256_unpacklo_epi8(rb, zero);
        __m256i g16 = _mm256_unpacklo_epi8(gb, zero);
        __m256i b16 = _mm256_unpacklo_epi8(bb, zero);

        r16 = _mm256_sll_epi16(_mm256_srl_epi16(r16, _mm_cvtsi32_si128(r_shr)), _mm_cvtsi32_si128(r_shift));
        g16 = _mm256_sll_epi16(_mm256_srl_epi16(g16, _mm_cvtsi32_si128(g_shr)), _mm_cvtsi32_si128(g_shift));
        b16 = _mm256_srl_epi16(b16, _mm_cvtsi32_si128(b_shr));

        __m256i ci = _mm256_or_si256(_mm256_or_si256(r16, g16), b16);
        __m256i packed = _mm256_packus_epi16(ci, zero);

        unsigned char tmp[32];
        _mm256_storeu_si256((__m256i *)tmp, packed);
        dst[x + 0] = tmp[0]; dst[x + 1] = tmp[1]; dst[x + 2] = tmp[2]; dst[x + 3] = tmp[3];
        dst[x + 4] = tmp[16]; dst[x + 5] = tmp[17]; dst[x + 6] = tmp[18]; dst[x + 7] = tmp[19];
    }
}

/*
 * AVX-512 variants. The arithmetic is identical to the AVX2 kernels so
 * both paths produce byte identical output; the wider vectors simply use
 * fewer instructions for the same work.
 */
__attribute__((target("avx512f,avx512bw,avx512dq,avx512vl,avx512vbmi"))) static void vert_acc_init_avx512(uint32_t *acc, const unsigned char *row, uint32_t w, int n) {
    const __m512i vw = _mm512_set1_epi32((int)w);
    int i = 0;
    for (; i + 16 <= n; i += 16) {
        __m128i v8 = _mm_loadu_si128((const __m128i *)(row + i));
        __m512i v = _mm512_cvtepu8_epi32(v8);
        _mm512_storeu_si512(acc + i, _mm512_mullo_epi32(v, vw));
    }
    for (; i + 8 <= n; i += 8) {
        __m128i v8 = _mm_loadl_epi64((const __m128i *)(row + i));
        __m256i v = _mm256_cvtepu8_epi32(v8);
        _mm256_storeu_si256((__m256i *)(acc + i), _mm256_mullo_epi32(v, _mm256_set1_epi32((int)w)));
    }
    for (; i < n; i++) {
        acc[i] = (uint32_t)row[i] * w;
    }
}

__attribute__((target("avx512f,avx512bw,avx512dq,avx512vl,avx512vbmi"))) static void vert_acc_avx512(uint32_t *acc, const unsigned char *row, uint32_t w, int n) {
    const __m512i vw = _mm512_set1_epi32((int)w);
    int i = 0;
    for (; i + 16 <= n; i += 16) {
        __m128i v8 = _mm_loadu_si128((const __m128i *)(row + i));
        __m512i v = _mm512_cvtepu8_epi32(v8);
        __m512i a = _mm512_loadu_si512(acc + i);
        _mm512_storeu_si512(acc + i, _mm512_add_epi32(a, _mm512_mullo_epi32(v, vw)));
    }
    for (; i + 8 <= n; i += 8) {
        __m128i v8 = _mm_loadl_epi64((const __m128i *)(row + i));
        __m256i v = _mm256_cvtepu8_epi32(v8);
        __m256i a = _mm256_loadu_si256((const __m256i *)(acc + i));
        _mm256_storeu_si256((__m256i *)(acc + i), _mm256_add_epi32(a, _mm256_mullo_epi32(v, _mm256_set1_epi32((int)w))));
    }
    for (; i < n; i++) {
        acc[i] += (uint32_t)row[i] * w;
    }
}

__attribute__((target("avx512f,avx512bw,avx512dq,avx512vl,avx512vbmi"))) static void norm_acc_avx512(unsigned char *dst, const unsigned int *acc, unsigned int inv, int n) {
    const __m512i vinv = _mm512_set1_epi32((int)inv);
    int i = 0;
    for (; i + 16 <= n; i += 16) {
        __m512i a = _mm512_loadu_si512(acc + i);
        __m512i pe = _mm512_mul_epu32(a, vinv);
        __m512i po = _mm512_mul_epu32(_mm512_srli_epi64(a, 32), _mm512_srli_epi64(vinv, 32));
        __m512i hi = _mm512_or_si512(_mm512_srli_epi64(pe, 32), _mm512_slli_epi64(_mm512_srli_epi64(po, 32), 32));
        __m512i lo = _mm512_mullo_epi32(a, vinv);
        __m512i q = _mm512_add_epi32(hi, _mm512_srli_epi32(lo, 31));
        __m128i q8 = _mm512_cvtepi32_epi8(q);
        _mm_storeu_si128((__m128i *)(dst + i), q8);
    }
    for (; i < n; i++) {
        dst[i] = (unsigned char)(((unsigned long long)acc[i] * inv + (1ULL << 31)) >> 32);
    }
}

__attribute__((target("avx512f,avx512bw,avx512dq,avx512vl,avx512vbmi"))) static void rgba_matrix_avx512(unsigned char *dst, const unsigned char *ty,
                               const unsigned char *tu, const unsigned char *tv, int n) {
    const __m512i vy = _mm512_set1_epi32(16);
    const __m512i vc = _mm512_set1_epi32(128);
    const __m512i k298 = _mm512_set1_epi32(298);
    const __m512i k409 = _mm512_set1_epi32(409);
    const __m512i k100 = _mm512_set1_epi32(100);
    const __m512i k208 = _mm512_set1_epi32(208);
    const __m512i k516 = _mm512_set1_epi32(516);
    const __m512i vrnd = _mm512_set1_epi32(128);
    const __m512i vzero = _mm512_setzero_si512();
    const __m512i v255 = _mm512_set1_epi32(255);
    const __m512i valpha = _mm512_set1_epi32((int)0xFF000000u);
    int i = 0;
    for (; i + 16 <= n; i += 16) {
        __m128i y8 = _mm_loadu_si128((const __m128i *)(ty + i));
        __m128i u8v = _mm_loadu_si128((const __m128i *)(tu + i));
        __m128i v8 = _mm_loadu_si128((const __m128i *)(tv + i));
        __m512i c298 = _mm512_mullo_epi32(_mm512_sub_epi32(_mm512_cvtepu8_epi32(y8), vy), k298);
        __m512i dd = _mm512_sub_epi32(_mm512_cvtepu8_epi32(u8v), vc);
        __m512i ee = _mm512_sub_epi32(_mm512_cvtepu8_epi32(v8), vc);
        __m512i r = _mm512_srai_epi32(_mm512_add_epi32(_mm512_add_epi32(c298, _mm512_mullo_epi32(ee, k409)), vrnd), 8);
        __m512i g = _mm512_srai_epi32(_mm512_add_epi32(_mm512_sub_epi32(_mm512_sub_epi32(c298, _mm512_mullo_epi32(dd, k100)), _mm512_mullo_epi32(ee, k208)), vrnd), 8);
        __m512i b = _mm512_srai_epi32(_mm512_add_epi32(_mm512_add_epi32(c298, _mm512_mullo_epi32(dd, k516)), vrnd), 8);
        r = _mm512_min_epi32(_mm512_max_epi32(r, vzero), v255);
        g = _mm512_min_epi32(_mm512_max_epi32(g, vzero), v255);
        b = _mm512_min_epi32(_mm512_max_epi32(b, vzero), v255);
        __m512i px = _mm512_or_si512(_mm512_or_si512(r, _mm512_slli_epi32(g, 8)),
                                     _mm512_or_si512(_mm512_slli_epi32(b, 16), valpha));
        _mm512_storeu_si512(dst + i * 4, px);
    }
    for (; i < n; i++) {
        int cc = (int)ty[i] - 16;
        int dd = (int)tu[i] - 128;
        int ee = (int)tv[i] - 128;
        int r = (298 * cc + 409 * ee + 128) >> 8;
        int g = (298 * cc - 100 * dd - 208 * ee + 128) >> 8;
        int b = (298 * cc + 516 * dd + 128) >> 8;
        if (r < 0) r = 0; else if (r > 255) r = 255;
        if (g < 0) g = 0; else if (g > 255) g = 255;
        if (b < 0) b = 0; else if (b > 255) b = 255;
        dst[i * 4 + 0] = (unsigned char)r;
        dst[i * 4 + 1] = (unsigned char)g;
        dst[i * 4 + 2] = (unsigned char)b;
        dst[i * 4 + 3] = 255;
    }
}

__attribute__((target("avx512f,avx512bw,avx512dq,avx512vl,avx512vbmi"))) static void quant_row_avx512(unsigned char *dst, const unsigned char *rgba, int w,
                             const unsigned char *dpos_r, const unsigned char *dneg_r,
                             const unsigned char *dpos_g, const unsigned char *dneg_g,
                             const unsigned char *dpos_b, const unsigned char *dneg_b,
                             int r_shr, int g_shr, int b_shr, int r_shift, int g_shift) {
    unsigned char idx[64], dpr[64], dnr[64], dpg[64], dng[64], dpb[64], dnb[64];
    for (int i = 0; i < 64; i++) {
        int c = i & 3;
        idx[i] = (unsigned char)(i < 16 ? i * 4 : 0);
        dpr[i] = dpos_r[c];
        dnr[i] = dneg_r[c];
        dpg[i] = dpos_g[c];
        dng[i] = dneg_g[c];
        dpb[i] = dpos_b[c];
        dnb[i] = dneg_b[c];
    }
    const __m512i idx_r = _mm512_loadu_si512(idx);
    const __m512i idx_g = _mm512_add_epi8(idx_r, _mm512_set1_epi8(1));
    const __m512i idx_b = _mm512_add_epi8(idx_r, _mm512_set1_epi8(2));
    const __m512i vdpr = _mm512_loadu_si512(dpr);
    const __m512i vdnr = _mm512_loadu_si512(dnr);
    const __m512i vdpg = _mm512_loadu_si512(dpg);
    const __m512i vdng = _mm512_loadu_si512(dng);
    const __m512i vdpb = _mm512_loadu_si512(dpb);
    const __m512i vdnb = _mm512_loadu_si512(dnb);
    int x = 0;
    for (; x + 16 <= w; x += 16) {
        __m512i px = _mm512_loadu_si512(rgba + x * 4);
        __m512i rb = _mm512_permutexvar_epi8(idx_r, px);
        __m512i gb = _mm512_permutexvar_epi8(idx_g, px);
        __m512i bb = _mm512_permutexvar_epi8(idx_b, px);
        rb = _mm512_subs_epu8(_mm512_adds_epu8(rb, vdpr), vdnr);
        gb = _mm512_subs_epu8(_mm512_adds_epu8(gb, vdpg), vdng);
        bb = _mm512_subs_epu8(_mm512_adds_epu8(bb, vdpb), vdnb);
        __m512i r16 = _mm512_cvtepu8_epi16(_mm512_castsi512_si256(rb));
        __m512i g16 = _mm512_cvtepu8_epi16(_mm512_castsi512_si256(gb));
        __m512i b16 = _mm512_cvtepu8_epi16(_mm512_castsi512_si256(bb));
        r16 = _mm512_sll_epi16(_mm512_srl_epi16(r16, _mm_cvtsi32_si128(r_shr)), _mm_cvtsi32_si128(r_shift));
        g16 = _mm512_sll_epi16(_mm512_srl_epi16(g16, _mm_cvtsi32_si128(g_shr)), _mm_cvtsi32_si128(g_shift));
        b16 = _mm512_srl_epi16(b16, _mm_cvtsi32_si128(b_shr));
        __m512i ci = _mm512_or_si512(_mm512_or_si512(r16, g16), b16);
        __m256i packed = _mm512_cvtepi16_epi8(ci);
        _mm256_storeu_si256((__m256i *)(dst + x), packed);
    }
    if (x < w) {
        quant_row_avx2(dst + x, rgba + x * 4, w - x, dpos_r, dneg_r, dpos_g, dneg_g,
                       dpos_b, dneg_b, r_shr, g_shr, b_shr, r_shift, g_shift);
    }
}

void quant_row_simd(unsigned char *dst, const unsigned char *rgba, int w,
                    const unsigned char *dpos_r, const unsigned char *dneg_r,
                    const unsigned char *dpos_g, const unsigned char *dneg_g,
                    const unsigned char *dpos_b, const unsigned char *dneg_b,
                    int r_shr, int g_shr, int b_shr, int r_shift, int g_shift) {
    if (cpu_simd_level() >= 3) {
        quant_row_avx512(dst, rgba, w, dpos_r, dneg_r, dpos_g, dneg_g, dpos_b, dneg_b,
                         r_shr, g_shr, b_shr, r_shift, g_shift);
        return;
    }
    quant_row_avx2(dst, rgba, w, dpos_r, dneg_r, dpos_g, dneg_g, dpos_b, dneg_b,
                   r_shr, g_shr, b_shr, r_shift, g_shift);
}

void vert_acc_init_simd(uint32_t *acc, const unsigned char *row, uint32_t w, int n) {
    if (cpu_simd_level() >= 3) { vert_acc_init_avx512(acc, row, w, n); return; }
    vert_acc_init_avx2(acc, row, w, n);
}

void vert_acc_simd(uint32_t *acc, const unsigned char *row, uint32_t w, int n) {
    if (cpu_simd_level() >= 3) { vert_acc_avx512(acc, row, w, n); return; }
    vert_acc_avx2(acc, row, w, n);
}

void norm_acc_simd(unsigned char *dst, const unsigned int *acc, unsigned int inv, int n) {
    if (cpu_simd_level() >= 3) { norm_acc_avx512(dst, acc, inv, n); return; }
    norm_acc_avx2(dst, acc, inv, n);
}

void rgba_matrix_simd(unsigned char *dst, const unsigned char *ty,
                      const unsigned char *tu, const unsigned char *tv, int n) {
    if (cpu_simd_level() >= 3) { rgba_matrix_avx512(dst, ty, tu, tv, n); return; }
    rgba_matrix_avx2(dst, ty, tu, tv, n);
}
