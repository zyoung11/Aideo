# 核心算法详解

### 1. 预处理：高斯差分 (Difference of Gaussians)

```glsl
  // 水平方向先做一次高斯模糊
  float PS_HorizontalBlur(...) {
      for (int x = -_KernelSize; x <= _KernelSize; ++x) {
          float2 gauss = float2(gaussian(_Sigma, x), gaussian(_Sigma * _SigmaScale, x));
          // 用两个不同 sigma 的高斯核分别卷积
      }
  }

  // 垂直方向再做一次，然后求差
  float PS_VerticalBlurAndDifference(...) {
      blur /= kernelSum;
      float D = (blur.x - _Tau * blur.y);  // 两个高斯核的差分
      D = (D >= _Threshold) ? 1 : 0;        // 二值化为边缘
  }
```

 原理：用两个不同宽度的高斯模糊做减法，得到边缘信息。这比 Sobel/Canny 更简单高效，且天然抗噪。

### 2. 边缘检测：深度 + 法线融合

```glsl
  float4 PS_EdgeDetect(...) {
      // 用 3x3 邻域计算深度差之和
      depthSum += abs(neighbor.w - center.w);  // 8 个方向
      if (_UseDepth && depthSum > _DepthThreshold)
          output = 1.0f;

      // 用 3x3 邻域计算法线差之和
      normalSum += abs(neighbor.rgb - center.rgb);
      if (_UseNormals && dot(normalSum, 1) > _NormalThreshold)
          output = 1.0f;

      // 最终：DoG 边缘与深度/法线边缘的差
      return saturate(abs(D - output));
  }
```

 创新点：结合了 DoG（亮度边缘）+ 深度边缘 + 法线边缘 三种信息，让 ASCII 线条在 3D 场景中也准确贴合几何轮廓。

### 3. Sobel 方向分析

```glsl
  // 水平方向 Sobel
  float Gx = 3 * lum1 + 0 * lum2 + -3 * lum3;
  float Gy = 3 + lum1 + 10 * lum2 + 3 * lum3;

  // 垂直方向 Sobel
  float Gx = 3 * grad1.x + 10 * grad2.x + 3 * grad3.x;
  float Gy = 3 * grad1.y + 0 * grad2.y + -3 * grad3.y;

  float theta = atan2(G.y, G.x);  // 梯度角度 [-π, π]
```

 Sobel 算子计算出每个像素的 梯度方向（theta），这决定了用哪种 ASCII 字符来表示这个边缘。

### 4. 计算着色器：核心合成逻辑

 这是整个算法的精华，使用 8×8 线程组 并行处理：

```glsl
  void CS_RenderASCII(uint3 tid, uint3 gid) {
      // Step 1: 根据 Sobel 角度分类边缘方向
      if (absTheta < 0.05f)       direction = 0;  // 垂直 |
      else if (absTheta > 0.9f)   direction = 0;  // 垂直 |
      else if (0.45 < absTheta < 0.55) direction = 1; // 水平 -
      else if (0.05 < absTheta < 0.45) direction = sign(theta) > 0 ? 3 : 2; // 对角线 / \

      // Step 2: 8×8 网格内投票，取最常见的方向
      edgeCount[gid.x + gid.y * 8] = direction;
      barrier();

      // 统计 64 个线程中哪个方向最多
      for (int i = 0; i < 64; ++i) buckets[edgeCount[i]]++;
      commonEdgeIndex = 出现次数最多的方向;

      // 如果该方向像素不足阈值，丢弃
      if (maxValue < _EdgeThreshold) commonEdgeIndex = -1;

      // Step 3: LUT 查找字符
      if (commonEdgeIndex >= 0 && _Edges) {
          // 边缘字符：从 edgesASCII.png 取
          localUV.x = (tid.x % 8) + quantizedEdge.x;
          localUV.y = 8 - (tid.y % 8);
          ascii = tex2Dfetch(EdgesASCII, localUV).r;
      } else if (_Fill) {
          // 填充字符：根据下采样的亮度从 fillASCII.png 取
          luminance = pow(downscaleInfo.w * _Exposure, _Attenuation);
          localUV.x = (tid.x % 8) + luminance * 80;
          localUV.y = tid.y % 8;
          ascii = tex2Dfetch(FillASCII, localUV).r;
      }

      // Step 4: 混合颜色 + 深度衰减雾效
      ascii = lerp(背景色, lerp(字符色, 原画面色, _BlendWithBase), ascii);
      fogFactor = exp2(-fogFactor * fogFactor);  // 指数衰减
      ascii = lerp(背景色, ascii, fogFactor);
  }
```

### 5. ASCII LUT 纹理

 项目使用两张纹理作为字符查找表：

- edgesASCII.png (40×8) — 4 种边缘字符（竖、横、对角线×2），每种 10 个灰度等级

- fillASCII.png (80×8) — 80 种填充字符，对应 10 个灰度等级 × 8 种字符
  
  字符按亮度从暗到亮排列，亮度越高选择越"重"的字符（如 @ → # → : → .）。
  
  #### 算法流程图
  
  ```
  原始帧
    │
    ├─ Pass 1: 亮度提取 ──────────────────→ LuminanceTex
    ├─ Pass 2: 下采样 (1/8) ──────────────→ DownscaleTex (用于填充亮度)
    │
    ├─ Pass 3: 水平高斯模糊 ──────────────→ AsciiPingTex
    ├─ Pass 4: 垂直高斯模糊 + DoG 差分 ───→ DoGTex (边缘二值图)
    │
    ├─ Pass 5: 法线计算 ──────────────────→ NormalsTex
    ├─ Pass 6: 边缘检测 (DoG+深度+法线) ──→ EdgesTex
    │
    ├─ Pass 7: Sobel 水平梯度 ────────────→ AsciiPingTex
    ├─ Pass 8: Sobel 垂直梯度 ────────────→ SobelTex (梯度角度θ)
    │
    └─ Pass 9: CS_RenderASCII (8×8 线程组)
         ├─ 角度→方向分类 (4 种)
         ├─ 8×8 网格投票取主方向
         ├─ LUT 查字符 (边缘/填充)
         ├─ 颜色混合 + 深度雾效
         └─ 输出 ASCII 画面
  ```
  
  关键设计亮点
1. 8×8 网格投票机制 — 每个 8×8 像素块内投票决定统一的方向，避免字符方向混乱
2. 多源边缘融合 — DoG + 深度 + 法线，3D 游戏场景下效果极佳
3. LUT 驱动 — 用纹理存储 ASCII 字符，GPU 直接查表，极高效
4. 计算着色器并行 — 用 groupshared 共享内存 + barrier() 同步，实现高效的网格内聚合
5. 深度感知 — 支持基于距离的雾效衰减，让远处 ASCII 自然淡出
