/**
 * ????? - sub2api ????
 *
 * ?????????????????/??????????
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

export function useDashboard(api, showToast, t) {
  const stats = ref({});
  let chartModel = null;
  let chartProtocol = null;
  let chartTraffic = null;

  async function load() {
    try {
      const d = await api('/stats', 'GET', null, t);
      stats.value = d;
      await nextTick();
      renderCharts(d);
    } catch (e) {
      if (e.message && !e.message.includes('unauthorized') && !e.message.includes('401')) {
        showToast(e.message, 'error');
      }
    }
  }

  function chartPalette() {
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

  function renderCharts(d) {
    renderTrafficChart(d);
    renderModelChart(d);
    renderProtocolChart(d);
  }

  function renderTrafficChart(d) {
    const el = document.getElementById('chartTraffic');
    if (!el) return;
    if (chartTraffic) chartTraffic.destroy();
    const palette = chartPalette();
    const points = d.timeline || [];
    chartTraffic = new Chart(el, {
      type: 'line',
      data: {
        labels: points.map((p) => (p.bucket || '').slice(5)),
        datasets: [
          {
            label: t('dashboard.requests'),
            data: points.map((p) => numberOrZero(p.total)),
            borderColor: palette.accent,
            backgroundColor: 'rgba(99,102,241,0.16)',
            fill: true,
            tension: 0.36,
            pointRadius: 2,
          },
          {
            label: t('dashboard.errors'),
            data: points.map((p) => numberOrZero(p.errors)),
            borderColor: palette.red,
            backgroundColor: 'rgba(239,68,68,0.08)',
            fill: true,
            tension: 0.36,
            pointRadius: 2,
          },
        ],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        interaction: { mode: 'index', intersect: false },
        plugins: {
          legend: { labels: { color: palette.text, usePointStyle: true } },
        },
        scales: {
          x: { ticks: { color: palette.text, maxRotation: 0 }, grid: { display: false } },
          y: { ticks: { color: palette.text }, grid: { color: palette.grid } },
        },
      },
    });
  }

  function renderModelChart(d) {
    const el = document.getElementById('chartModel');
    if (!el) return;
    if (chartModel) chartModel.destroy();
    const palette = chartPalette();
    const rows = (d.by_model || []).slice(0, 10);
    chartModel = new Chart(el, {
      type: 'bar',
      data: {
        labels: rows.map((m) => m.model || 'unknown'),
        datasets: [
          {
            label: t('dashboard.requests'),
            data: rows.map((m) => numberOrZero(m.count)),
            backgroundColor: 'rgba(99,102,241,0.62)',
            borderColor: palette.accent,
            borderWidth: 1,
            borderRadius: 8,
          },
        ],
      },
      options: {
        indexAxis: 'y',
        responsive: true,
        maintainAspectRatio: false,
        plugins: { legend: { display: false } },
        scales: {
          x: { ticks: { color: palette.text }, grid: { color: palette.grid } },
          y: { ticks: { color: palette.text, font: { size: 11 } }, grid: { display: false } },
        },
      },
    });
  }

  function renderProtocolChart(d) {
    const el = document.getElementById('chartProtocol');
    if (!el) return;
    if (chartProtocol) chartProtocol.destroy();
    const palette = chartPalette();
    const rows = d.by_protocol || [];
    const colors = [palette.accent, palette.green, palette.yellow, palette.blue, palette.red];
    chartProtocol = new Chart(el, {
      type: 'doughnut',
      data: {
        labels: rows.map((p) => p.protocol || 'unknown'),
        datasets: [{ data: rows.map((p) => numberOrZero(p.count)), backgroundColor: colors, borderWidth: 0 }],
      },
      options: {
        responsive: true,
        maintainAspectRatio: false,
        cutout: '62%',
        plugins: {
          legend: { position: 'bottom', labels: { color: palette.text, padding: 14, usePointStyle: true } },
        },
      },
    });
  }

  function compactNumber(v) {
    const n = numberOrZero(v);
    if (n >= 1_000_000) return (n / 1_000_000).toFixed(2) + 'M';
    if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
    return n.toLocaleString();
  }

  function formatTokens(v) {
    const n = numberOrZero(v);
    if (n >= 1_000_000_000) return (n / 1_000_000_000).toFixed(2) + 'B';
    if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
    if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
    return Math.round(n).toLocaleString();
  }

  function formatCost(v) {
    const n = numberOrZero(v);
    if (n !== 0 && Math.abs(n) < 0.0001) {
      return '$' + n.toFixed(10).replace(/0+$/, '').replace(/\.$/, '');
    }
    return '$' + n.toFixed(4);
  }

  function formatPercent(v) {
    return (numberOrZero(v) * 100).toFixed(1) + '%';
  }

  function formatDuration(ms) {
    const n = numberOrZero(ms);
    if (n >= 1000) return (n / 1000).toFixed(2) + 's';
    return Math.round(n) + 'ms';
  }

  function healthScore(d) {
    const success = numberOrZero(d?.success_rate);
    const avg = numberOrZero(d?.avg_duration_ms);
    const latencyPenalty = Math.min(25, Math.max(0, (avg - 800) / 80));
    const score = Math.round(success * 100 - latencyPenalty);
    return Math.max(0, Math.min(100, score || (numberOrZero(d?.total_calls) ? 70 : 100)));
  }

  function statusLabel(code) {
    const n = Number(code);
    if (n >= 500) return '5xx';
    if (n >= 400) return '4xx';
    if (n >= 300) return '3xx';
    if (n >= 200) return '2xx';
    return String(code || '?');
  }

  function recentErrors(d) {
    return (d?.recent || []).filter((r) => Number(r.status_code) >= 400).slice(0, 6);
  }

  return {
    stats,
    load,
    compactNumber,
    formatTokens,
    formatCost,
    formatPercent,
    formatDuration,
    healthScore,
    statusLabel,
    recentErrors,
  };
}
