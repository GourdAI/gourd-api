/**
 * Centralized platform color definitions.
 *
 * All components that need platform-specific styling should import from here
 * instead of defining their own color mappings.
 */

export type Platform =
  | 'anthropic'
  | 'openai'
  | 'antigravity'
  | 'gemini'
  | 'grok'
  | 'kimi'
  | 'zhipu'
  | 'deepseek'
  | 'minimax'
  | 'opencode_go'
  | 'workbuddy'
  | 'qoder'
  | 'trae'
  | 'composite'

// ── Badge (bg + text + border, for inline badges with border) ───────
const BADGE: Record<Platform, string> = {
  anthropic: 'bg-orange-500/10 text-orange-600 border-orange-500/30 dark:text-orange-400',
  openai: 'bg-green-500/10 text-green-600 border-green-500/30 dark:text-green-400',
  antigravity: 'bg-brandpurple-500/10 text-brandpurple-600 border-brandpurple-500/30 dark:text-brandpurple-400',
  gemini: 'bg-brandblue-500/10 text-brandblue-600 border-brandblue-500/30 dark:text-brandblue-400',
  grok: 'bg-zinc-800/10 text-zinc-800 border-zinc-800/30 dark:bg-zinc-500/10 dark:text-zinc-200 dark:border-zinc-500/30',
  kimi: 'bg-pink-500/10 text-pink-600 border-pink-500/30 dark:text-pink-400',
  zhipu: 'bg-brandindigo-500/10 text-brandindigo-600 border-brandindigo-500/30 dark:text-brandindigo-400',
  deepseek: 'bg-teal-500/10 text-teal-600 border-teal-500/30 dark:text-teal-400',
  minimax: 'bg-rose-500/10 text-rose-600 border-rose-500/30 dark:text-rose-400',
  opencode_go: 'bg-amber-500/10 text-amber-700 border-amber-500/30 dark:text-amber-300',
  workbuddy: 'bg-brandviolet-500/10 text-brandviolet-600 border-brandviolet-500/30 dark:text-brandviolet-400',
  qoder: 'bg-brandpurple-500/10 text-brandpurple-600 border-brandpurple-500/30 dark:text-brandpurple-400',
  trae: 'bg-brandsky-500/10 text-brandsky-600 border-brandsky-500/30 dark:text-brandsky-400',
  composite: 'bg-brandcyan-500/10 text-brandcyan-700 border-brandcyan-500/30 dark:text-brandcyan-300',
}
const BADGE_DEFAULT = 'bg-slate-500/10 text-slate-600 border-slate-500/30 dark:text-slate-400'

// ── Light badge (softer bg, no border) ──────────────────────────────
const BADGE_LIGHT: Record<Platform, string> = {
  anthropic: 'bg-orange-500/10 text-orange-600 dark:bg-orange-500/10 dark:text-orange-300',
  openai: 'bg-green-500/10 text-green-600 dark:bg-green-500/10 dark:text-green-300',
  antigravity: 'bg-brandpurple-500/10 text-brandpurple-600 dark:bg-brandpurple-500/10 dark:text-brandpurple-300',
  gemini: 'bg-brandblue-500/10 text-brandblue-600 dark:bg-brandblue-500/10 dark:text-brandblue-300',
  grok: 'bg-zinc-800/10 text-zinc-800 dark:bg-zinc-500/10 dark:text-zinc-200',
  kimi: 'bg-pink-500/10 text-pink-600 dark:bg-pink-500/10 dark:text-pink-300',
  zhipu: 'bg-brandindigo-500/10 text-brandindigo-600 dark:bg-brandindigo-500/10 dark:text-brandindigo-300',
  deepseek: 'bg-teal-500/10 text-teal-600 dark:bg-teal-500/10 dark:text-teal-300',
  minimax: 'bg-rose-500/10 text-rose-600 dark:bg-rose-500/10 dark:text-rose-300',
  opencode_go: 'bg-amber-500/10 text-amber-700 dark:bg-amber-500/10 dark:text-amber-300',
  workbuddy: 'bg-brandviolet-500/10 text-brandviolet-600 dark:bg-brandviolet-500/10 dark:text-brandviolet-300',
  qoder: 'bg-brandpurple-500/10 text-brandpurple-600 dark:bg-brandpurple-500/10 dark:text-brandpurple-300',
  trae: 'bg-brandsky-500/10 text-brandsky-600 dark:bg-brandsky-500/10 dark:text-brandsky-300',
  composite: 'bg-brandcyan-500/10 text-brandcyan-700 dark:bg-brandcyan-500/10 dark:text-brandcyan-300',
}

// ── Border ──────────────────────────────────────────────────────────
const BORDER: Record<Platform, string> = {
  anthropic: 'border-orange-500/20 dark:border-orange-500/20',
  openai: 'border-green-500/20 dark:border-green-500/20',
  antigravity: 'border-brandpurple-500/20 dark:border-brandpurple-500/20',
  gemini: 'border-brandblue-500/20 dark:border-brandblue-500/20',
  grok: 'border-zinc-800/20 dark:border-zinc-500/20',
  kimi: 'border-pink-500/20 dark:border-pink-500/20',
  zhipu: 'border-brandindigo-500/20 dark:border-brandindigo-500/20',
  deepseek: 'border-teal-500/20 dark:border-teal-500/20',
  minimax: 'border-rose-500/20 dark:border-rose-500/20',
  opencode_go: 'border-amber-500/20 dark:border-amber-500/20',
  workbuddy: 'border-brandviolet-500/20 dark:border-brandviolet-500/20',
  qoder: 'border-brandpurple-500/20 dark:border-brandpurple-500/20',
  trae: 'border-brandsky-500/20 dark:border-brandsky-500/20',
  composite: 'border-brandcyan-500/20 dark:border-brandcyan-500/20',
}
const BORDER_DEFAULT = 'border-gray-200 dark:border-dark-700'

// ── Border strong (higher-contrast platform tint, e.g. plaza group cards) ──
const BORDER_STRONG: Record<Platform, string> = {
  anthropic: 'border-orange-500/35 dark:border-orange-500/30',
  openai: 'border-green-500/35 dark:border-green-500/30',
  antigravity: 'border-brandpurple-500/35 dark:border-brandpurple-500/30',
  gemini: 'border-brandblue-500/35 dark:border-brandblue-500/30',
  grok: 'border-zinc-800/35 dark:border-zinc-500/35',
  kimi: 'border-pink-500/35 dark:border-pink-500/30',
  zhipu: 'border-brandindigo-500/35 dark:border-brandindigo-500/30',
  deepseek: 'border-teal-500/35 dark:border-teal-500/30',
  minimax: 'border-rose-500/35 dark:border-rose-500/30',
  opencode_go: 'border-amber-500/35 dark:border-amber-500/30',
  workbuddy: 'border-brandviolet-500/35 dark:border-brandviolet-500/30',
  qoder: 'border-brandpurple-500/35 dark:border-brandpurple-500/30',
  trae: 'border-brandsky-500/35 dark:border-brandsky-500/30',
  composite: 'border-brandcyan-500/35 dark:border-brandcyan-500/30',
}
const BORDER_STRONG_DEFAULT = 'border-gray-300 dark:border-dark-600'

// ── Accent (single raw color per platform; consumers derive washes/tints
//    from it via CSS color-mix, e.g. plaza paid-price zone) ──
const ACCENT: Record<Platform, string> = {
  anthropic: '#f97316', // orange-500
  openai: '#22c55e', // green-500
  antigravity: '#a855f7', // purple-500
  gemini: '#3b82f6', // blue-500
  grok: '#71717a', // zinc-500
  kimi: '#ec4899', // pink-500
  zhipu: '#6366f1', // indigo-500
  deepseek: '#14b8a6', // teal-500
  minimax: '#f43f5e', // rose-500
  opencode_go: '#f59e0b', // amber-500
  workbuddy: '#8b5cf6', // violet-500
  qoder: '#7c3aed', // Qoder brand purple (#7C3AED)
  trae: '#0284c7', // Trae brand blue-cyan (sky-600; cyan/teal/blue are taken by composite/deepseek/gemini)
  composite: '#06b6d4', // cyan-500
}
const ACCENT_DEFAULT = '#14b8a6' // primary-500 (teal)

// ── Accent bar (gradient) ───────────────────────────────────────────
const ACCENT_BAR: Record<Platform, string> = {
  anthropic: 'bg-gradient-to-r from-orange-400 to-orange-500',
  openai: 'bg-gradient-to-r from-emerald-400 to-emerald-500',
  antigravity: 'bg-gradient-to-r from-brandpurple-400 to-brandpurple-500',
  gemini: 'bg-gradient-to-r from-brandblue-400 to-brandblue-500',
  grok: 'bg-gradient-to-r from-zinc-700 to-zinc-900',
  kimi: 'bg-gradient-to-r from-pink-400 to-pink-500',
  zhipu: 'bg-gradient-to-r from-brandindigo-400 to-brandindigo-500',
  deepseek: 'bg-gradient-to-r from-teal-400 to-teal-500',
  minimax: 'bg-gradient-to-r from-rose-400 to-rose-500',
  opencode_go: 'bg-gradient-to-r from-amber-400 to-amber-500',
  workbuddy: 'bg-gradient-to-r from-brandviolet-400 to-brandviolet-500',
  qoder: 'bg-gradient-to-r from-brandpurple-400 to-brandpurple-500',
  trae: 'bg-gradient-to-r from-brandsky-400 to-brandsky-500',
  composite: 'bg-gradient-to-r from-slate-500 to-brandcyan-500',
}
const ACCENT_BAR_DEFAULT = 'bg-gradient-to-r from-primary-400 to-primary-500'

// ── Text (price, icon) ─────────────────────────────────────────────
const TEXT: Record<Platform, string> = {
  anthropic: 'text-orange-600 dark:text-orange-400',
  openai: 'text-emerald-600 dark:text-emerald-400',
  antigravity: 'text-brandpurple-600 dark:text-brandpurple-400',
  gemini: 'text-brandblue-600 dark:text-brandblue-400',
  grok: 'text-zinc-800 dark:text-zinc-200',
  kimi: 'text-pink-600 dark:text-pink-400',
  zhipu: 'text-brandindigo-600 dark:text-brandindigo-400',
  deepseek: 'text-teal-600 dark:text-teal-400',
  minimax: 'text-rose-600 dark:text-rose-400',
  opencode_go: 'text-amber-700 dark:text-amber-300',
  workbuddy: 'text-brandviolet-600 dark:text-brandviolet-400',
  qoder: 'text-brandpurple-600 dark:text-brandpurple-400',
  trae: 'text-brandsky-600 dark:text-brandsky-400',
  composite: 'text-brandcyan-700 dark:text-brandcyan-300',
}
const TEXT_DEFAULT = 'text-primary-600 dark:text-primary-400'

// ── Icon (check mark etc.) ──────────────────────────────────────────
const ICON: Record<Platform, string> = {
  anthropic: 'text-orange-500 dark:text-orange-400',
  openai: 'text-emerald-500 dark:text-emerald-400',
  antigravity: 'text-brandpurple-500 dark:text-brandpurple-400',
  gemini: 'text-brandblue-500 dark:text-brandblue-400',
  grok: 'text-zinc-800 dark:text-zinc-200',
  kimi: 'text-pink-500 dark:text-pink-400',
  zhipu: 'text-brandindigo-500 dark:text-brandindigo-400',
  deepseek: 'text-teal-500 dark:text-teal-400',
  minimax: 'text-rose-500 dark:text-rose-400',
  opencode_go: 'text-amber-500 dark:text-amber-300',
  workbuddy: 'text-brandviolet-500 dark:text-brandviolet-400',
  qoder: 'text-brandpurple-500 dark:text-brandpurple-400',
  trae: 'text-brandsky-500 dark:text-brandsky-400',
  composite: 'text-brandcyan-600 dark:text-brandcyan-300',
}
const ICON_DEFAULT = 'text-primary-500 dark:text-primary-400'

// ── Button (solid bg) ───────────────────────────────────────────────
const BUTTON: Record<Platform, string> = {
  anthropic: 'bg-orange-500 text-white hover:bg-orange-600 active:bg-orange-700 dark:bg-orange-500/80 dark:hover:bg-orange-500',
  openai: 'bg-green-600 text-white hover:bg-green-700 active:bg-green-800 dark:bg-green-600/80 dark:hover:bg-green-600',
  antigravity: 'bg-brandpurple-500 text-white hover:bg-brandpurple-600 active:bg-brandpurple-700 dark:bg-brandpurple-500/80 dark:hover:bg-brandpurple-500',
  gemini: 'bg-brandblue-500 text-white hover:bg-brandblue-600 active:bg-brandblue-700 dark:bg-brandblue-500/80 dark:hover:bg-brandblue-500',
  grok: 'bg-zinc-800 text-white hover:bg-zinc-900 active:bg-black dark:bg-zinc-700 dark:hover:bg-zinc-600',
  kimi: 'bg-pink-500 text-white hover:bg-pink-600 active:bg-pink-700 dark:bg-pink-500/80 dark:hover:bg-pink-500',
  zhipu: 'bg-brandindigo-500 text-white hover:bg-brandindigo-600 active:bg-brandindigo-700 dark:bg-brandindigo-500/80 dark:hover:bg-brandindigo-500',
  deepseek: 'bg-teal-500 text-white hover:bg-teal-600 active:bg-teal-700 dark:bg-teal-500/80 dark:hover:bg-teal-500',
  minimax: 'bg-rose-500 text-white hover:bg-rose-600 active:bg-rose-700 dark:bg-rose-500/80 dark:hover:bg-rose-500',
  opencode_go: 'bg-amber-500 text-white hover:bg-amber-600 active:bg-amber-700 dark:bg-amber-500/80 dark:hover:bg-amber-500',
  workbuddy: 'bg-brandviolet-500 text-white hover:bg-brandviolet-600 active:bg-brandviolet-700 dark:bg-brandviolet-500/80 dark:hover:bg-brandviolet-500',
  qoder: 'bg-brandpurple-500 text-white hover:bg-brandpurple-600 active:bg-brandpurple-700 dark:bg-brandpurple-500/80 dark:hover:bg-brandpurple-500',
  trae: 'bg-brandsky-600 text-white hover:bg-brandsky-700 active:bg-brandsky-800 dark:bg-brandsky-600/80 dark:hover:bg-brandsky-600',
  composite: 'bg-brandcyan-700 text-white hover:bg-brandcyan-800 active:bg-brandcyan-900 dark:bg-brandcyan-600 dark:hover:bg-brandcyan-500',
}
const BUTTON_DEFAULT = 'bg-primary-500 text-white hover:bg-primary-600 dark:bg-primary-600 dark:hover:bg-primary-500'

// ── Discount badge ──────────────────────────────────────────────────
const DISCOUNT: Record<Platform, string> = {
  anthropic: 'bg-orange-100 text-orange-700 dark:bg-orange-900/40 dark:text-orange-300',
  openai: 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-300',
  antigravity: 'bg-brandpurple-100 text-brandpurple-700 dark:bg-brandpurple-900/40 dark:text-brandpurple-300',
  gemini: 'bg-brandblue-100 text-brandblue-700 dark:bg-brandblue-900/40 dark:text-brandblue-300',
  grok: 'bg-zinc-100 text-zinc-800 dark:bg-zinc-800 dark:text-zinc-200',
  kimi: 'bg-pink-100 text-pink-700 dark:bg-pink-900/40 dark:text-pink-300',
  zhipu: 'bg-brandindigo-100 text-brandindigo-700 dark:bg-brandindigo-900/40 dark:text-brandindigo-300',
  deepseek: 'bg-teal-100 text-teal-700 dark:bg-teal-900/40 dark:text-teal-300',
  minimax: 'bg-rose-100 text-rose-700 dark:bg-rose-900/40 dark:text-rose-300',
  opencode_go: 'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300',
  workbuddy: 'bg-brandviolet-100 text-brandviolet-700 dark:bg-brandviolet-900/40 dark:text-brandviolet-300',
  qoder: 'bg-brandpurple-100 text-brandpurple-700 dark:bg-brandpurple-900/40 dark:text-brandpurple-300',
  trae: 'bg-brandsky-100 text-brandsky-800 dark:bg-brandsky-900/40 dark:text-brandsky-300',
  composite: 'bg-brandcyan-100 text-brandcyan-800 dark:bg-brandcyan-900/40 dark:text-brandcyan-300',
}
const DISCOUNT_DEFAULT = 'bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-300'

// ── Header gradient (subscription confirm) ─────────────────────────
const GRADIENT: Record<Platform, string> = {
  anthropic: 'from-orange-500 to-orange-600',
  openai: 'from-emerald-500 to-emerald-600',
  antigravity: 'from-brandpurple-500 to-brandpurple-600',
  gemini: 'from-brandblue-500 to-brandblue-600',
  grok: 'from-zinc-700 to-zinc-900',
  kimi: 'from-pink-500 to-pink-600',
  zhipu: 'from-brandindigo-500 to-brandindigo-600',
  deepseek: 'from-teal-500 to-teal-600',
  minimax: 'from-rose-500 to-rose-600',
  opencode_go: 'from-amber-500 to-amber-600',
  workbuddy: 'from-brandviolet-500 to-brandviolet-600',
  qoder: 'from-brandpurple-500 to-brandpurple-600',
  trae: 'from-brandsky-500 to-brandsky-600',
  composite: 'from-slate-600 to-brandcyan-600',
}
const GRADIENT_DEFAULT = 'from-primary-500 to-primary-600'

// ── Header text (light text on gradient bg) ────────────────────────
const GRADIENT_TEXT: Record<Platform, string> = {
  anthropic: 'text-orange-100',
  openai: 'text-emerald-100',
  antigravity: 'text-brandpurple-100',
  gemini: 'text-brandblue-100',
  grok: 'text-zinc-100',
  kimi: 'text-pink-100',
  zhipu: 'text-brandindigo-100',
  deepseek: 'text-teal-100',
  minimax: 'text-rose-100',
  opencode_go: 'text-amber-100',
  workbuddy: 'text-brandviolet-100',
  qoder: 'text-brandpurple-100',
  trae: 'text-brandsky-100',
  composite: 'text-brandcyan-100',
}
const GRADIENT_TEXT_DEFAULT = 'text-primary-100'

const GRADIENT_SUBTEXT: Record<Platform, string> = {
  anthropic: 'text-orange-200',
  openai: 'text-emerald-200',
  antigravity: 'text-brandpurple-200',
  gemini: 'text-brandblue-200',
  grok: 'text-zinc-300',
  kimi: 'text-pink-200',
  zhipu: 'text-brandindigo-200',
  deepseek: 'text-teal-200',
  minimax: 'text-rose-200',
  opencode_go: 'text-amber-200',
  workbuddy: 'text-brandviolet-200',
  qoder: 'text-brandpurple-200',
  trae: 'text-brandsky-200',
  composite: 'text-brandcyan-200',
}
const GRADIENT_SUBTEXT_DEFAULT = 'text-primary-200'

// ── Public API ──────────────────────────────────────────────────────

function isPlatform(p: string): p is Platform {
  return (
    p === 'anthropic' ||
    p === 'openai' ||
    p === 'antigravity' ||
    p === 'gemini' ||
    p === 'grok' ||
    p === 'kimi' ||
    p === 'zhipu' ||
    p === 'deepseek' ||
    p === 'minimax' ||
    p === 'opencode_go' ||
    p === 'workbuddy' ||
    p === 'qoder' ||
    p === 'trae' ||
    p === 'composite'
  )
}

export function platformBadgeClass(p: string): string {
  return isPlatform(p) ? BADGE[p] : BADGE_DEFAULT
}

export function platformBadgeLightClass(p: string): string {
  return isPlatform(p) ? BADGE_LIGHT[p] : BADGE_DEFAULT
}

export function platformBorderClass(p: string): string {
  return isPlatform(p) ? BORDER[p] : BORDER_DEFAULT
}

export function platformBorderStrongClass(p: string): string {
  return isPlatform(p) ? BORDER_STRONG[p] : BORDER_STRONG_DEFAULT
}

export function platformAccentColor(p: string): string {
  return isPlatform(p) ? ACCENT[p] : ACCENT_DEFAULT
}

export function platformAccentBarClass(p: string): string {
  return isPlatform(p) ? ACCENT_BAR[p] : ACCENT_BAR_DEFAULT
}

export function platformTextClass(p: string): string {
  return isPlatform(p) ? TEXT[p] : TEXT_DEFAULT
}

export function platformIconClass(p: string): string {
  return isPlatform(p) ? ICON[p] : ICON_DEFAULT
}

export function platformButtonClass(p: string): string {
  return isPlatform(p) ? BUTTON[p] : BUTTON_DEFAULT
}

export function platformDiscountClass(p: string): string {
  return isPlatform(p) ? DISCOUNT[p] : DISCOUNT_DEFAULT
}

export function platformGradientClass(p: string): string {
  return isPlatform(p) ? GRADIENT[p] : GRADIENT_DEFAULT
}

export function platformGradientTextClass(p: string): string {
  return isPlatform(p) ? GRADIENT_TEXT[p] : GRADIENT_TEXT_DEFAULT
}

export function platformGradientSubtextClass(p: string): string {
  return isPlatform(p) ? GRADIENT_SUBTEXT[p] : GRADIENT_SUBTEXT_DEFAULT
}

export function platformLabel(p: string): string {
  switch (p) {
    case 'anthropic': return 'Anthropic'
    case 'openai': return 'OpenAI'
    case 'antigravity': return 'Antigravity'
    case 'gemini': return 'Gemini'
    case 'grok': return 'Grok'
    case 'kimi': return 'Kimi'
    case 'zhipu': return 'Zhipu GLM'
    case 'deepseek': return 'DeepSeek'
    case 'minimax': return 'MiniMax'
    case 'opencode_go': return 'OpenCode'
    case 'workbuddy': return 'WorkBuddy'
    case 'qoder': return 'Qoder'
    case 'trae': return 'Trae'
    case 'composite': return 'Composite'
    default: return p || 'API'
  }
}
