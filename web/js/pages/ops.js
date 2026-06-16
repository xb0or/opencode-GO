/**
 * ??????
 *
 * ????????? usage_logs ? pool health ???????? Ops ???
 */

const { ref, nextTick } = Vue;

function cssVar(name, fallback) {
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  return value || fallback;
}

function numberOrZero(v) {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

export function useOps(api, showToast, t) {
  const stats = ref({});
  const health = ref({});
  const loading = ref(false);
  const lastUpdated = ref(null);
  let throughputChart = null;
  let latencyChart = null;
  let errorChart = null;

  async function load() {
    loading.value = true;
    try {
      const [s, h] = await Promise.all([
        api('/stats', 'GET', null, t),
        api('/health', 'GET', null, t),
      ]);
      stats.value = s;
      health.value = h;
      lastUpdated.value = new Date();
      await nextTick();
      renderCharts(s);
    } catch (e) {
      if (e.message && !e.message.includes('unauthorized') && !e.message.includes('401')) {
        showToast(e.message, 'error');
      }
    } finally {
      loading.value = false;
    }
  }

  function palette() {
    return {
      text: cssVar('--muted', '#8892b0'),
      grid: 'rgba(136,146,176,0.18)',
      accent: cssVar('--accent', '#6366f1'),
      green: cssVar('--green', '#10b981'),
      red: cssVar('--red', '#ef4444'),
      yellow: cssVar('--yellow', '#f59e0b'),
      blue: cssVar('--blue', '#3b82f6'),
    };
  }

  function renderCharts(s) {
    renderThroughput(s);
    renderLatency(s);
    renderErrors(s);
  }

  function renderThroughput(s) {
    const el = document.getElementById('chartOpsThroughput');
    if (!el) return;
    if (throughputChart) throughputChart.destroy();
    const p = palette();
    const rows = s.timeline || [];
    throughputChart = new Chart(el, {
      type: 'bar',
      data: {
        labels: rows.map((r) => (r.bucket || '').slice(5)),
        datasets: [
          { label: t('ops.success'), data: rows.map((r) => numberOrZero(r.success)), backgroundColor: 'rgba(16,185,129,0.62)', borderRadius: 6 },
          { label: t('ops.errors'), data: rows.map((r) => numberOrZero(r.errors)), backgroundColor: 'rgba(239,68,68,0.58)', borderRadius: 6 },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend: { labels: { color: p.text, usePointStyle: true } } },
        scales: {
          x: { stacked: true, ticks: { color: p.text, maxRotation: 0 }, grid: { display: false } },
          y: { stacked: true, ticks: { color: p.text }, grid: { color: p.grid } },
        },
      },
    });
  }

  function renderLatency(s) {
    const el = document.getElementById('chartLatency');
    if (!el) return;
    if (latencyChart) latencyChart.destroy();
    const p = palette();
    const rows = s.latency_buckets || [];
    latencyChart = new Chart(el, {
      type: 'bar',
      data: {
        labels: rows.map((r) => r.range),
        datasets: [{ label: t('ops.requests'), data: rows.map((r) => numberOrZero(r.count)), backgroundColor: 'rgba(59,130,246,0.62)', borderRadius: 8 }],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { color: p.text }, grid: { display: false } },
          y: { ticks: { color: p.text }, grid: { color: p.grid } },
        },
      },
    });
  }

  function renderErrors(s) {
    const el = document.getElementById('chartErrors');
    if (!el) return;
    if (errorChart) errorChart.destroy();
    const p = palette();
    const rows = (s.by_status || []).filter((r) => Number(r.status_code) >= 400);
    errorChart = new Chart(el, {
      type: 'doughnut',
      data: {
        labels: rows.map((r) => r.status_code),
        datasets: [{ data: rows.map((r) => numberOrZero(r.count)), backgroundColor: [p.red, p.yellow, p.blue, p.accent], borderWidth: 0 }],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        cutout: '60%',
        plugins: { legend: { position: 'bottom', labels: { color: p.text, usePointStyle: true } } },
      },
    });
  }

  function formatPercent(v) {
    return (numberOrZero(v) * 100).toFixed(1) + '%';
  }

  function formatDuration(ms) {
    const n = numberOrZero(ms);
    if (n >= 1000) return (n / 1000).toFixed(2) + 's';
    return Math.round(n) + 'ms';
  }

  function formatRate(v) {
    const n = numberOrZero(v);
    return n >= 10 ? n.toFixed(1) : n.toFixed(2);
  }

  function pool() {
    return health.value?.pools?.go || {};
  }

  function healthScore() {
    const s = stats.value || {};
    const p = pool();
    const successScore = numberOrZero(s.success_rate) * 70;
    const availabilityScore = numberOrZero(p.total) ? (numberOrZero(p.available) / numberOrZero(p.total)) * 30 : 30;
    return Math.max(0, Math.min(100, Math.round(successScore + availabilityScore)));
  }

  function recentErrors() {
    return (stats.value?.recent || []).filter((r) => Number(r.status_code) >= 400).slice(0, 12);
  }

  function updatedLabel(locale) {
    if (!lastUpdated.value) return '?';
    return lastUpdated.value.toLocaleTimeString(locale === 'zh' ? 'zh-CN' : 'en-US');
  }

  return {
    stats,
    health,
    loading,
    load,
    formatPercent,
    formatDuration,
    formatRate,
    pool,
    healthScore,
    recentErrors,
    updatedLabel,
  };
}
