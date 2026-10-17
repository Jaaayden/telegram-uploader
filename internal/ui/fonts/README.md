# 内置字体

`NotoSansSC-Regular.subset.otf` 和 `NotoSansSC-Bold.subset.otf` 是 [Noto Sans SC](https://github.com/notofonts/noto-cjk)（SIL Open Font License 1.1，见 `OFL.txt`）的裁剪版本。

Fyne 默认字体不含中文，会对每个汉字分别回退到系统里“恰好包含该字”的字体，同一行文字因此混用多种字体、字重和基线。应用内置同一套中文字体后，常用汉字始终来自同一字体。

裁剪范围：ASCII、常用标点与全角符号、GB2312 的 6763 个汉字，以及源码中出现过的所有非 ASCII 字符。不含 U+00B7（间隔号 ·）：思源黑体中它是全角，用作拉丁文分隔符时间距过大，交给 Fyne 默认字体绘制。范围之外的字符仍按 Fyne 的原有方式回退到系统字体。

重新生成（需要 `pip install fonttools`，并先从 noto-cjk 仓库下载 `Sans/SubsetOTF/SC/NotoSansSC-{Regular,Bold}.otf`）：

```bash
pyftsubset NotoSansSC-Regular.otf --unicodes-file=unicodes.txt \
  --output-file=NotoSansSC-Regular.subset.otf \
  --layout-features='kern,liga,locl,ccmp,mark,mkmk' --no-hinting --desubroutinize
```
