/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{vue,js,ts,jsx,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        // 主色调 - Aurora Mint 极光薄荷（偏绿、无蓝相，避开「天青/死亡蓝紫」）
        primary: {
          50: '#ecfdf6',
          100: '#cff9e8',
          200: '#9ff1d3',
          300: '#5ee5ba',
          400: '#22d3a3',
          500: '#00be93',
          600: '#008068',
          700: '#036853',
          800: '#085445',
          900: '#0b453a',
          950: '#012b24'
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
        // 一处消除全站底面的「蓝灰」观感（此类引用全仓近 3200 处）。
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
        // ── 蓝紫色系收编 ──
        // ── 蓝紫色系收编 ──
        // 全仓有 2000+ 处 blue/sky/cyan/indigo/purple/violet/fuchsia 语义类（信息态、
        // 链接、模型层级、运营面板等）。在此整体重映射：一处收编、可逆、零遗漏，
        // 风险远低于逐文件改写。
        // 500 档色相：blue 189° / sky 175° / cyan 152° / indigo 77° / purple 312° /
        // violet 325° / fuchsia 298°，均跳出 200°~290° 蓝紫带。
        // 收编时需同时避开语义色：blue 不可归薄荷（与 emerald/green 成功态同色），
        // violet 不可归玫红（与 red 错误态同色）。
        blue: {
          50: '#f2f7f8',
          100: '#e0ecee',
          200: '#c2dade',
          300: '#91bec5',
          400: '#5d9aa4',
          500: '#3f7d88',
          600: '#336670',
          700: '#2c535c',
          800: '#27454d',
          900: '#233b42',
          950: '#132125'
        },
        sky: {
          50: '#edfdfb',
          100: '#d0f9f4',
          200: '#a2f1e7',
          300: '#64e2d4',
          400: '#2bcbbb',
          500: '#12ada0',
          600: '#0a8c83',
          700: '#0b6f69',
          800: '#0d5854',
          900: '#0f4946',
          950: '#032a28'
        },
        // 翡翠：cyan 族归此，与 primary(166°) 差 14°，靠饱和度与明度区分
        cyan: {
          50: '#ecfdf5',
          100: '#d1fadf',
          200: '#a6f4c5',
          300: '#6ce9a6',
          400: '#32d583',
          500: '#12b76a',
          600: '#098a50',
          700: '#087044',
          800: '#095c39',
          900: '#094c31',
          950: '#052e1d'
        },
        indigo: {
          50: '#f7fee7',
          100: '#ecfccb',
          200: '#dceab3',
          300: '#c0d983',
          400: '#a3c551',
          500: '#86a92c',
          600: '#66861e',
          700: '#4e671c',
          800: '#40521c',
          900: '#36461b',
          950: '#1a2508'
        },
        // purple 归 312° 紫红：既不在蓝紫带内，也不与 amber（原生 38°）同相
        purple: {
          50: '#fdf4fb',
          100: '#fae0f5',
          200: '#f4b8e8',
          300: '#eb7ad4',
          400: '#e042c1',
          500: '#ca21a8',
          600: '#a71b8b',
          700: '#881671',
          800: '#72135f',
          900: '#601050',
          950: '#35092c'
        },
        // violet 归 325° 品红：不可取玫红，会与 red（错误态）同色
        violet: {
          50: '#fdf2fa',
          100: '#fce8f5',
          200: '#fad0ec',
          300: '#f6a6dd',
          400: '#ef6fc3',
          500: '#e0429f',
          600: '#c42a83',
          700: '#a22069',
          800: '#851e56',
          900: '#711d49',
          950: '#440a28'
        },
        // fuchsia 归 298° 洋紫：不可直接用 pink 原阶（会与原生 pink 同色）
        fuchsia: {
          50: '#fdf4fd',
          100: '#f9e0fa',
          200: '#f2b8f4',
          300: '#e77aeb',
          400: '#d945de',
          500: '#c223c7',
          600: '#a01da5',
          700: '#831886',
          800: '#6e1471',
          900: '#5d115f',
          950: '#330934'
        },
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
        glow: '0 0 20px rgba(0, 190, 147, 0.22)',
        'glow-lg': '0 0 40px rgba(0, 190, 147, 0.32)',
        card: '0 1px 3px rgba(0, 0, 0, 0.04), 0 1px 2px rgba(0, 0, 0, 0.06)',
        'card-hover': '0 10px 40px rgba(0, 0, 0, 0.08)',
        'inner-glow': 'inset 0 1px 0 rgba(255, 255, 255, 0.1)'
      },
      backgroundImage: {
        'gradient-radial': 'radial-gradient(var(--tw-gradient-stops))',
        'gradient-primary': 'linear-gradient(135deg, #00be93 0%, #009e7a 100%)',
        'gradient-dark': 'linear-gradient(135deg, #27272a 0%, #09090b 100%)',
        'gradient-glass':
          'linear-gradient(135deg, rgba(255,255,255,0.1) 0%, rgba(255,255,255,0.05) 100%)',
        'mesh-gradient':
          'radial-gradient(at 40% 20%, rgba(0, 190, 147, 0.12) 0px, transparent 50%), radial-gradient(at 80% 0%, rgba(34, 211, 163, 0.08) 0px, transparent 50%), radial-gradient(at 0% 50%, rgba(0, 158, 122, 0.08) 0px, transparent 50%)'
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
          '0%': { opacity: '0', transform: 'translateX(20px)' },
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
          '0%': { boxShadow: '0 0 20px rgba(0, 190, 147, 0.22)' },
          '100%': { boxShadow: '0 0 30px rgba(0, 190, 147, 0.36)' }
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
