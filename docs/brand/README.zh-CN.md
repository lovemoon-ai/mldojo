[English](README.md) | 简体中文

# MLDojo Logo

<img src="lockup.svg" alt="MLDojo" height="56">

## 概念：四叠半

道场（Dojo）的地面铺的是榻榻米。标志取自最经典的「四叠半」铺法：四张整叠像风车一样，围着中间的半叠。

| 元素 | 含义 |
|---|---|
| 四张整叠 | 四类算力：本地 / SSH / 命令行队列 / 外部云。MLDojo 不绑定框架，也不自研调度，只负责把它们组织起来 |
| 中央半叠（蓝） | Run。平台的一切都围绕一次训练：组织好、跑起来、记录下来 |
| 顺时针风车 | 迭代循环：提交 → 训练 → 对比 → 再提交 |
| 没有四角交汇的点 | 这种铺法叫「祝儀敷き」，是吉祥的铺法，寓意结构稳定 |

## 构造

<img src="../../web/public/icons/icon.svg" alt="App icon" height="96">

- 3×3 网格：单元 U=80，间隙 G=24，圆角 R=12。整叠 184×80，半叠 80×80，标志整体 288×288。
- App 图标：512 画布，底色 `#18181b`，圆角 112。标志居中（112–400），落在 PWA maskable 安全区内（半径 40%）。
- 字标：Geist SemiBold（SIL OFL 1.1），字距 -0.02em，已转成路径。大写字母高度是标志高度的 50%，标志和字标的间距是标志宽度的 1/3。

## 颜色

与 `web/app/globals.css` 的 `--primary` 和 `--chart-1` 一致。

| 用途 | 浅色背景 | 深色背景 |
|---|---|---|
| 叠 / 字标 | `#18181b` | `#fafafa` |
| Run（半叠） | `#2a78d6` | `#3987e5` |

## 文件

| 文件 | 用途 |
|---|---|
| `lockup.svg` / `lockup-dark.svg` | 标志 + 字标，透明底 |
| `showcase.png` | 展示图 |
| `web/public/icons/mark.svg` / `mark-dark.svg` | 标志，透明底，用于浅色 / 深色背景（Web 侧边栏、登录页） |
| `web/public/icons/icon.svg`、`icon-*.png` | App 图标 / PWA 图标 / favicon，也是 Conductor 项目卡片的图标 |

`web/public/icons/*` 由 `npm run icons`（`web/scripts/gen-icons.mjs`）生成。

## 使用规则

- 只有中央半叠用蓝色。只能用单色的场合（印刷、刻印）整体用一种颜色。
- 不要旋转或镜像（风车方向固定为顺时针），不要改变叠的比例和间隙。
- 最小尺寸：标志 16px，lockup 高 20px。
- 四周留白至少 1U（约为标志宽度的 28%）。
