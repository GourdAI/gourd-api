/** @type {import('tailwindcss').Config} */

/* ============================================================================
 * 铂金黑（Platinum Obsidian）主题色板
 *
 * 取证来源：gourdwork.com/css/styles.css（29,482 字符，一手抓取）
 *   --bg:#050607 / --text:#f4f4f5 / --text-dim:#a3a3ab / --text-faint:#6f6f78
 *   边框 rgba(255,255,255,.08)，卡片 140deg #3f3f46→#18181b，
 *   标题金属渐变 110deg #fff→#9ca3af→#fff
 *   全站唯一彩色 --ok:#34d399 被注释钉死为「仅用于运行中/成功语义」
 *   → 结论：该风格不用彩色做强调，强调由「明度 + 白色低透明边框」承载。
 *
 * 本仓审计来源：852 个 .vue/.ts，21,546 处色值引用
 *   gray/dark/slate/zinc = 13,758 处（63.9%）→ 已与官网同源，本次不动
 *   primary              =  1,890 处（8.8%）→ 本次重铸的主体
 *   blue/sky/cyan/indigo/purple/violet/fuchsia = 1,862 处 → 装饰彩度收编
 *   red/rose/amber/orange/emerald/green/teal   ≈ 状态语义 → 保持彩度
 *
 * 关键判定：装饰族是否必须彼此可辨？
 *   实测 48 个同屏多用族的文件里，色值一律紧邻文字标签
 *   （如 label:'Opus 4.5' 配 bg-purple-100；if(level==='ultra') 配 purple），
 *   颜色是「强化」而非「唯一信息载体」→ 色相身份可弃，明度阶梯可留。
 *   明度分层比色相分层更优：高档位=更深，天然表达严重度。
 *
 * 硬约束（全部经 src/utils/__tests__/themePlatinum.spec.ts 断言）：
 *   C1 50→950 严格单调变暗
 *   C2 每个色值 8bit 通道极差 ≤ 6（保证是「冷灰」而不是「蓝」；
 *      官网自身中性极差仅 2~8：#050607→2、#18181b→3、#a3a3ab→8）
 *   C3 ≥500 档白字对比度 ≥ 4.5（500 档作填充时有 102 处配白字）
 *   C4 200~400 档在暗底 #0a0a0b 上对比度 ≥ 4.5（暗色模式正文用这档）
 *   C5 徽标对 600-on-100、暗色徽标对 300-on-900 可读
 *   C6 相邻族在 600/700/800 档不得取值相同
 *
 * 负向约束：品牌与装饰族不得出现彩度 >8% 的蓝/靛/紫（色相 200°~290°）。
 * ==========================================================================*/

export default {
  content: ['./index.html', './src/**/*.{vue,js,ts,jsx,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        // 主色调 - 铂金冷银。无彩度品牌，强调靠明度反差。
        // 语义映射（保持与改造前一致的档位分工）：
        //   50/100  浅色徽标底 · 200/300 暗色模式正文与分割 · 400 暗色模式正文
        //   500     信息态文字/图标（白字填充的最低安全档）
        //   600     亮色模式正文 · 700/800 深银面 · 900/950 反转主按钮面
        // 注意：主按钮不用 500/600，而是走 900(亮)/200(暗) 的极值反转 ——
        // 500/600 与 gray-500/600 明度接近，做按钮面会「像灰片而不像主行动点」；
        // 近黑底 + 白字才是这套风格里唯一有分量的强调。见 style.css .btn-primary。
        primary: {
          50: '#f5f7fa',
          100: '#eef0f3',
          200: '#dee0e3',
          300: '#c5c7ca',
          400: '#a2a4a7',
          500: '#6e7073',
          600: '#56585b',
          700: '#414346',
          800: '#2c2e31',
          900: '#1c1e21',
          950: '#0b0d10'
        },
        // 辅助色 - Obsidian 曜石中性灰（纯中性、零蓝相）
        accent: {
          50: '#fafafa',
          100: '#f4f4f5',
          200: '#e4e4e7',
          300: '#d4d4d8',
          400: '#a1a1aa',
          500: '#71717a',
          600: '#52525b',
          700: '#3f3f46',
          800: '#27272a',
          900: '#18181b',
          950: '#09090b'
        },
        // 深色模式背景 - 与 accent 同源（高级黑）
        dark: {
          50: '#fafafa',
          100: '#f4f4f5',
          200: '#e4e4e7',
          300: '#d4d4d8',
          400: '#a1a1aa',
          500: '#71717a',
          600: '#52525b',
          700: '#3f3f46',
          800: '#27272a',
          900: '#18181b',
          950: '#09090b'
        },
        // Tailwind 内置 gray/slate 自带 220° 蓝相，重映射为纯中性后，
        // 一处消除全站底面的「蓝灰」观感（此类引用全仓近 3,200 处）。
        // 这组值与官网中性几乎逐位相同（#f4f4f5/#3f3f46/#18181b/#a3a3ab），
        // 因此本轮不再改动 —— 上一版把界面搞脏的不是中性，是彩色超载。
        gray: {
          50: '#fafafa',
          100: '#f4f4f5',
          200: '#e4e4e7',
          300: '#d4d4d8',
          400: '#a1a1aa',
          500: '#71717a',
          600: '#52525b',
          700: '#3f3f46',
          800: '#27272a',
          900: '#18181b',
          950: '#09090b'
        },
        slate: {
          50: '#fafafa',
          100: '#f4f4f5',
          200: '#e4e4e7',
          300: '#d4d4d8',
          400: '#a1a1aa',
          500: '#71717a',
          600: '#52525b',
          700: '#3f3f46',
          800: '#27272a',
          900: '#18181b',
          950: '#09090b'
        },
        // ── 装饰彩度收编为冷银明度阶梯 ──
        // blue/sky/cyan/indigo/purple/violet/fuchsia 共 1,862 处。
        // 50~500 档各族取值相同（徽标底、浅面必须读成同一种材质，
        // 否则会看到七种不同的「灰白」在打架）；仅 600~950 档按族下探，
        // 保留一条极轻的层级差。排序 sky(最浅) → indigo(最深)。
        sky: {
          50: '#f5f7fa',
          100: '#eef0f3',
          200: '#dee0e3',
          300: '#c5c7ca',
          400: '#a2a4a7',
          500: '#6e7073',
          600: '#525457',
          700: '#3e4043',
          800: '#2a2c2f',
          900: '#1a1c1f',
          950: '#0a0c0f'
        },
        cyan: {
          50: '#f5f7fa',
          100: '#eef0f3',
          200: '#dee0e3',
          300: '#c5c7ca',
          400: '#a2a4a7',
          500: '#6e7073',
          600: '#4e5053',
          700: '#3b3d40',
          800: '#282a2d',
          900: '#181a1d',
          950: '#090b0e'
        },
        blue: {
          50: '#f5f7fa',
          100: '#eef0f3',
          200: '#dee0e3',
          300: '#c5c7ca',
          400: '#a2a4a7',
          500: '#6e7073',
          600: '#4a4c4f',
          700: '#383a3d',
          800: '#25272a',
          900: '#16181b',
          950: '#080a0d'
        },
        fuchsia: {
          50: '#f5f7fa',
          100: '#eef0f3',
          200: '#dee0e3',
          300: '#c5c7ca',
          400: '#a2a4a7',
          500: '#6e7073',
          600: '#46484b',
          700: '#35373a',
          800: '#222427',
          900: '#141619',
          950: '#07090c'
        },
        violet: {
          50: '#f5f7fa',
          100: '#eef0f3',
          200: '#dee0e3',
          300: '#c5c7ca',
          400: '#a2a4a7',
          500: '#6e7073',
          600: '#424447',
          700: '#323437',
          800: '#202225',
          900: '#121417',
          950: '#06080b'
        },
        purple: {
          50: '#f5f7fa',
          100: '#eef0f3',
          200: '#dee0e3',
          300: '#c5c7ca',
          400: '#a2a4a7',
          500: '#6e7073',
          600: '#3e4043',
          700: '#2f3134',
          800: '#1d1f22',
          900: '#101215',
          950: '#05070a'
        },
        // teal 也是绿色系（内置 #14b8a6，色相 174°），全仓 136 处。
        // 它与 emerald/green 成功态同相，既不能当品牌也不能当信息态，归入冷银。
        teal: {
          50: '#f5f7fa',
          100: '#eef0f3',
          200: '#dee0e3',
          300: '#c5c7ca',
          400: '#a2a4a7',
          500: '#6e7073',
          600: '#36383b',
          700: '#292b2e',
          800: '#181a1d',
          900: '#0c0e11',
          // 950 比 cyan-950 (#090b0e) 再深一档，不进入「页面底色」区。
          950: '#080a0d'
        },
        indigo: {
          50: '#f5f7fa',
          100: '#eef0f3',
          200: '#dee0e3',
          300: '#c5c7ca',
          400: '#a2a4a7',
          500: '#6e7073',
          600: '#3a3c3f',
          700: '#2c2e31',
          800: '#1b1d20',
          900: '#0e1013',
          950: '#040609'
        },
        // ── 图表专用低饱和分类色（viz*）──
        // 数据可视化的分类色不能走灰阶：多系列折线/堆叠柱在灰阶下不可辨。
        // 因此单开命名空间，保留色相身份但把彩度压到 25~40%，并与主题的
        // 「冷银」基调对齐（钢蓝/松绿/黄铜/梅紫/陶土），只给图表用。
        // 业务主题不得引用 viz*，反之图表不得用 primary/gray（会全灰）。
        viz1: { 400: '#8fa3b8', 500: '#6f8ba6', 600: '#52718c', 700: '#3c5670' },
        viz2: { 400: '#7fae96', 500: '#4c8f79', 600: '#3a7361', 700: '#2a5849' },
        viz3: { 400: '#cfa96f', 500: '#b58a4e', 600: '#946d39', 700: '#73532a' },
        viz4: { 400: '#b791ad', 500: '#9a6f92', 600: '#7c5575', 700: '#5e3f58' },
        viz5: { 400: '#a8b07a', 500: '#87986a', 600: '#6a7a50', 700: '#4f5c3a' },
        viz6: { 400: '#bd9a83', 500: '#a1775c', 600: '#825d46', 700: '#634633' },
        viz7: { 400: '#9aa7b5', 500: '#6d7480', 600: '#525c6b', 700: '#3d4653' },
        // ── 平台品牌识别色（刻意不受上方收编影响）──
        // Gemini 本征蓝、Qoder 本征紫、DeepSeek 本征青等属于「第三方品牌事实」，
        // 不是本站主题色。platformColors.ts 同时持有 raw hex（ACCENT）与类名（BORDER/
        // ACCENT_BAR）两套色源，若让这些类名跟随重映射，同一平台会出现
        // 「徽标仍蓝、边框变青」的分叉。因此为品牌色开独立命名空间，
        // 值等于 Tailwind 3.4 内置原色阶。仅身份文件可用 brand*，业务主题不得引用。
        brandblue: {
          50: '#eff6ff',
          100: '#dbeafe',
          200: '#bfdbfe',
          300: '#93c5fd',
          400: '#60a5fa',
          500: '#3b82f6',
          600: '#2563eb',
          700: '#1d4ed8',
          800: '#1e40af',
          900: '#1e3a8a',
          950: '#172554'
        },
        brandpurple: {
          50: '#faf5ff',
          100: '#f3e8ff',
          200: '#e9d5ff',
          300: '#d8b4fe',
          400: '#c084fc',
          500: '#a855f7',
          600: '#9333ea',
          700: '#7e22ce',
          800: '#6b21a8',
          900: '#581c87',
          950: '#3b0764'
        },
        brandindigo: {
          50: '#eef2ff',
          100: '#e0e7ff',
          200: '#c7d2fe',
          300: '#a5b4fc',
          400: '#818cf8',
          500: '#6366f1',
          600: '#4f46e5',
          700: '#4338ca',
          800: '#3730a3',
          900: '#312e81',
          950: '#1e1b4b'
        },
        brandviolet: {
          50: '#f5f3ff',
          100: '#ede9fe',
          200: '#ddd6fe',
          300: '#c4b5fd',
          400: '#a78bfa',
          500: '#8b5cf6',
          600: '#7c3aed',
          700: '#6d28d9',
          800: '#5b21b6',
          900: '#4c1d95',
          950: '#2e1065'
        },
        brandsky: {
          50: '#f0f9ff',
          100: '#e0f2fe',
          200: '#bae6fd',
          300: '#7dd3fc',
          400: '#38bdf8',
          500: '#0ea5e9',
          600: '#0284c7',
          700: '#0369a1',
          800: '#075985',
          900: '#0c4a6e',
          950: '#082f49'
        },
        brandcyan: {
          50: '#ecfeff',
          100: '#cffafe',
          200: '#a5f3fc',
          300: '#67e8f9',
          400: '#22d3ee',
          500: '#06b6d4',
          600: '#0891b2',
          700: '#0e7490',
          800: '#155e75',
          900: '#164e63',
          950: '#083344'
        },
        brandteal: {
          50: '#f0fdfa',
          100: '#ccfbf1',
          200: '#99f6e4',
          300: '#5eead4',
          400: '#2dd4bf',
          500: '#14b8a6',
          600: '#0d9488',
          700: '#0f766e',
          800: '#115e59',
          900: '#134e4a',
          950: '#042f2e'
        }
      },
      fontFamily: {
        sans: [
          'system-ui',
          '-apple-system',
          'BlinkMacSystemFont',
          'Segoe UI',
          'Roboto',
          'Helvetica Neue',
          'Arial',
          'PingFang SC',
          'Hiragino Sans GB',
          'Microsoft YaHei',
          'sans-serif'
        ],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Monaco', 'Consolas', 'monospace']
      },
      boxShadow: {
        glass: '0 8px 32px rgba(0, 0, 0, 0.08)',
        'glass-sm': '0 4px 16px rgba(0, 0, 0, 0.06)',
        // 光晕一律改为白色低透明 —— 官网的「发光」是光，不是色素。
        // 上一版这里是 rgba(0,190,147,.22) 薄荷光，是「绿墙」观感的主要来源之一。
        glow: '0 0 20px rgba(255, 255, 255, 0.07)',
        'glow-lg': '0 0 40px rgba(255, 255, 255, 0.11)',
        card: '0 1px 3px rgba(0, 0, 0, 0.04), 0 1px 2px rgba(0, 0, 0, 0.06)',
        'card-hover': '0 10px 40px rgba(0, 0, 0, 0.08)',
        'inner-glow': 'inset 0 1px 0 rgba(255, 255, 255, 0.1)'
      },
      backgroundImage: {
        'gradient-radial': 'radial-gradient(var(--tw-gradient-stops))',
        // 金属渐变：照搬官网标题处理（白 → 灰 → 白），铂金黑的核心识别符
        'gradient-primary': 'linear-gradient(110deg, #ffffff 20%, #9ca3af 50%, #ffffff 80%)',
        'gradient-dark': 'linear-gradient(140deg, #3f3f46 0%, #18181b 100%)',
        'gradient-glass':
          'linear-gradient(135deg, rgba(255,255,255,0.10) 0%, rgba(255,255,255,0.05) 100%)',
        // 氛围光：白/钢斑，α ≤ 0.05，绝不进蓝紫，也不留薄荷
        'mesh-gradient':
          'radial-gradient(at 40% 20%, rgba(255,255,255,0.055) 0px, transparent 50%), radial-gradient(at 80% 0%, rgba(163,163,171,0.045) 0px, transparent 50%), radial-gradient(at 0% 50%, rgba(120,128,140,0.04) 0px, transparent 50%)'
      },
      animation: {
        'fade-in': 'fadeIn 0.3s ease-out',
        'slide-up': 'slideUp 0.3s ease-out',
        'slide-down': 'slideDown 0.3s ease-out',
        'slide-in-right': 'slideInRight 0.3s ease-out',
        'scale-in': 'scaleIn 0.2s ease-out',
        'pulse-slow': 'pulse 3s cubic-bezier(0.4, 0, 0.6, 1) infinite',
        shimmer: 'shimmer 2s linear infinite',
        glow: 'glow 2s ease-in-out infinite alternate'
      },
      keyframes: {
        fadeIn: {
          '0%': { opacity: '0' },
          '100%': { opacity: '1' }
        },
        slideUp: {
          '0%': { opacity: '0', transform: 'translateY(10px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' }
        },
        slideDown: {
          '0%': { opacity: '0', transform: 'translateY(-10px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' }
        },
        slideInRight: {
          '0%': { opacity: '0', transform: 'translateX(10px)' },
          '100%': { opacity: '1', transform: 'translateX(0)' }
        },
        scaleIn: {
          '0%': { opacity: '0', transform: 'scale(0.95)' },
          '100%': { opacity: '1', transform: 'scale(1)' }
        },
        shimmer: {
          '0%': { backgroundPosition: '-200% 0' },
          '100%': { backgroundPosition: '200% 0' }
        },
        glow: {
          '0%': { boxShadow: '0 0 20px rgba(255, 255, 255, 0.07)' },
          '100%': { boxShadow: '0 0 30px rgba(255, 255, 255, 0.11)' }
        }
      },
      backdropBlur: {
        xs: '2px'
      },
      borderRadius: {
        '4xl': '2rem'
      }
    }
  },
  plugins: []
}
