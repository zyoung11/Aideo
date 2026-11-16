#include <immintrin.h>

/*
 * quant_row_avx2 quantizes 8 RGBA pixels per iteration using 256-bit
 * registers. Each channel has its own scaled Bayer dither pair in lane
 * order: bytes 0..3 for pixels 0..3 and bytes 16..19 for pixels 4..7.
 */
void quant_row_avx2(unsigned char *dst, const unsigned char *rgba, int w,
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
