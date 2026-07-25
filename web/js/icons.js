/**
 * Lucide icon registry.
 *
 * The admin UI renders icons through Vue's v-html directive. Keeping the
 * Lucide paths in one registry gives every surface the same 24px grid,
 * 1.8px stroke and rounded line treatment without adding a runtime package.
 */
const lucide = (content, className = "") =>
  `<svg class="lucide ${className}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${content}</svg>`;

export const icons = {
  logo: lucide('<path d="m13 2-9 11h8l-1 9 9-12h-8z"/>', "lucide-zap"),
  dashboard: lucide(
    '<rect width="7" height="9" x="3" y="3" rx="1"/><rect width="7" height="5" x="14" y="3" rx="1"/><rect width="7" height="9" x="14" y="12" rx="1"/><rect width="7" height="5" x="3" y="16" rx="1"/>',
    "lucide-layout-dashboard",
  ),
  ops: lucide(
    '<path d="M22 12h-4l-3 9L9 3l-3 9H2"/>',
    "lucide-activity",
  ),
  usage: lucide(
    '<path d="M3 3v18h18"/><path d="M18 17V9"/><path d="M13 17V5"/><path d="M8 17v-3"/>',
    "lucide-chart-no-axes-column-increasing",
  ),
  key: lucide(
    '<circle cx="7.5" cy="15.5" r="5.5"/><path d="m21 2-9.6 9.6"/><path d="m15.5 7.5 3 3L22 7l-3-3"/>',
    "lucide-key-round",
  ),
  token: lucide(
    '<path d="M3 6h18"/><path d="M7 12h10"/><path d="M10 18h4"/>',
    "lucide-rows-3",
  ),
  model: lucide(
    '<path d="M12 5a3 3 0 1 0-5.997.142"/><path d="M18 11a3 3 0 1 0-1.858-5.997"/><path d="M17 19a3 3 0 1 0 1-5.83"/><path d="M6 19a3 3 0 1 1-1-5.83"/><path d="M12 19a3 3 0 1 1 5.997-.142"/><path d="M12 5a3 3 0 1 1 5.997.142"/><path d="M12 8v8"/><path d="m8.5 9.5 7 5"/><path d="m15.5 9.5-7 5"/>',
    "lucide-brain-circuit",
  ),
  mapping: lucide(
    '<circle cx="5" cy="6" r="3"/><path d="M5 9v6"/><circle cx="5" cy="18" r="3"/><path d="M12 6h5a2 2 0 0 1 2 2v3"/><path d="m16 9 3 3 3-3"/><path d="M12 18h5a2 2 0 0 0 2-2v-4"/>',
    "lucide-git-compare-arrows",
  ),
  chevron: lucide('<path d="m9 18 6-6-6-6"/>', "lucide-chevron-right"),
  sun: lucide(
    '<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>',
    "lucide-sun",
  ),
  moon: lucide('<path d="M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9"/>', "lucide-moon"),
  globe: lucide(
    '<path d="M2 12h20"/><path d="M12 2a15.3 15.3 0 0 1 0 20"/><path d="M12 2a15.3 15.3 0 0 0 0 20"/><circle cx="12" cy="12" r="10"/>',
    "lucide-languages",
  ),
  github: lucide(
    '<path d="M15 22v-4a4.8 4.8 0 0 0-1-3.5c3.28-.36 6.72-1.61 6.72-7.25A5.65 5.65 0 0 0 19.22 3.3 5.27 5.27 0 0 0 19.08 0S17.9-.38 15 1.48a13.38 13.38 0 0 0-7 0C5.1-.38 3.92 0 3.92 0a5.27 5.27 0 0 0-.14 3.3 5.65 5.65 0 0 0-1.5 3.95c0 5.63 3.44 6.88 6.72 7.25A4.8 4.8 0 0 0 8 18v4"/><path d="M8 19c-3 .92-3-1.5-4-2"/>',
    "lucide-github",
  ),
  refresh: lucide(
    '<path d="M20 11a8.1 8.1 0 0 0-15.5-2M4 4v5h5"/><path d="M4 13a8.1 8.1 0 0 0 15.5 2M20 20v-5h-5"/>',
    "lucide-refresh-cw",
  ),
  layers: lucide(
    '<path d="m12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83z"/><path d="m22 12.5-9.17 4.17a2 2 0 0 1-1.66 0L2 12.5"/><path d="m22 17.5-9.17 4.17a2 2 0 0 1-1.66 0L2 17.5"/>',
    "lucide-layers-3",
  ),
  wallet: lucide(
    '<path d="M19 7V4a1 1 0 0 0-1-1H5a3 3 0 0 0 0 6h15a1 1 0 0 1 1 1v4h-4a2 2 0 0 0 0 4h4v2a1 1 0 0 1-1 1H5a3 3 0 0 1-3-3V6"/><path d="M16 14h.01"/>',
    "lucide-wallet-cards",
  ),
  coins: lucide(
    '<circle cx="8" cy="8" r="6"/><path d="M18.09 10.37A6 6 0 1 1 10.34 18"/><path d="M7 6h1v4"/><path d="m16.71 13.88.7.71-2.82 2.82"/>',
    "lucide-coins",
  ),
  gauge: lucide(
    '<path d="m12 14 4-4"/><path d="M3.34 19a10 10 0 1 1 17.32 0"/>',
    "lucide-gauge",
  ),
  timer: lucide(
    '<line x1="10" x2="14" y1="2" y2="2"/><line x1="12" x2="15" y1="14" y2="11"/><circle cx="12" cy="14" r="8"/>',
    "lucide-timer",
  ),
  shield: lucide(
    '<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/><path d="m9 12 2 2 4-4"/>',
    "lucide-shield-check",
  ),
  radio: lucide(
    '<path d="M4.9 19.1C1 15.2 1 8.8 4.9 4.9"/><path d="M7.8 16.2a6 6 0 0 1 0-8.5"/><circle cx="12" cy="12" r="2"/><path d="M16.2 7.8a6 6 0 0 1 0 8.5"/><path d="M19.1 4.9C23 8.8 23 15.1 19.1 19"/>',
    "lucide-radio-tower",
  ),
  warning: lucide(
    '<path d="m21.73 18-8-14a2 2 0 0 0-3.46 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>',
    "lucide-triangle-alert",
  ),
  logout: lucide(
    '<path d="M10 17l5-5-5-5"/><path d="M15 12H3"/><path d="M15 3h4a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2h-4"/>',
    "lucide-log-out",
  ),
  check: lucide('<path d="m9 12 2 2 4-4"/><circle cx="12" cy="12" r="10"/>', "lucide-circle-check"),
  close: lucide('<path d="M18 6 6 18"/><path d="m6 6 12 12"/>', "lucide-x"),
};
