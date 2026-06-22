/**
 * 使用记录页面组合式函数。
 * 负责筛选、全局统计、详情弹窗和计费展示格式化。
 */

const { reactive, ref, computed } = Vue;

function numberOrZero(v) {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

function positive(v) {
  return numberOrZero(v) > 0;
}

function round2(v) {
  const n = numberOrZero(v);
  return Number.isInteger(n) ? String(n) : n.toFixed(2).replace(/0+$/, '').replace(/\.$/, '');
}

function dateTimeLocalValue(date) {
  const pad = (n) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export function useUsage(api, showToast, t) {
  const logs = ref([]);
  const loading = ref(false);
  const selected = ref(null);
  const apiSummary = ref(null);
  const filters = reactive({ q: '', status: '', protocol: '', stream: '', start: '', end: '', pageSize: 25 });
  const pagination = reactive({ page: 1, page_size: 25, total: 0 });

  const pageSummary = computed(() => {
    const rows = logs.value || [];
    const errors = rows.filter((r) => Number(r.status_code) >= 400).length;
    const avg = rows.length ? rows.reduce((sum, r) => sum + numberOrZero(r.duration_ms), 0) / rows.length : 0;
    return {
      total_calls: rows.length,
      visible: rows.length,
      errors,
      error_calls: errors,
      success: rows.length - errors,
      success_calls: rows.length - errors,
      avg_duration_ms: avg,
      input_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.input_tokens), 0),
      output_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.output_tokens), 0),
      reasoning_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.reasoning_tokens), 0),
      cache_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.cache_read_tokens || r.cache_tokens), 0),
      cache_read_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.cache_read_tokens || r.cache_tokens), 0),
      cache_creation_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.cache_creation_tokens), 0),
      total_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.total_tokens), 0),
      total_cost: rows.reduce((sum, r) => sum + numberOrZero(r.total_cost), 0),
      actual_cost: rows.reduce((sum, r) => sum + numberOrZero(r.actual_cost || r.total_cost), 0),
      account_cost: rows.reduce((sum, r) => sum + numberOrZero(r.account_cost || r.actual_cost || r.total_cost), 0),
      rpm: 0,
      tpm: 0,
    };
  });

  const summary = computed(() => apiSummary.value || pageSummary.value);

  function buildQuery() {
    const p = new URLSearchParams();
    p.set('page', String(pagination.page));
    p.set('page_size', String(filters.pageSize || pagination.page_size));
    if (filters.q.trim()) p.set('q', filters.q.trim());
    if (filters.status) p.set('status', filters.status);
    if (filters.protocol.trim()) p.set('protocol', filters.protocol.trim());
    if (filters.stream) p.set('stream', filters.stream);
    if (filters.start) p.set('start', new Date(filters.start).toISOString());
    if (filters.end) p.set('end', new Date(filters.end).toISOString());
    p.set('sort_by', 'created_at');
    p.set('sort_order', 'desc');
    return '?' + p.toString();
  }

  async function load() {
    loading.value = true;
    try {
      const d = await api('/usage' + buildQuery(), 'GET', null, t);
      logs.value = d.items || [];
      apiSummary.value = d.summary || null;
      pagination.total = d.total || 0;
      pagination.page = d.page || pagination.page;
      pagination.page_size = d.page_size || filters.pageSize || pagination.page_size;
    } catch (e) {
      if (e.message && !e.message.includes('unauthorized') && !e.message.includes('401')) {
        showToast(e.message, 'error');
      }
    } finally {
      loading.value = false;
    }
  }

  function apply() {
    pagination.page = 1;
    pagination.page_size = Number(filters.pageSize) || 25;
    load();
  }

  function reset() {
    filters.q = '';
    filters.status = '';
    filters.protocol = '';
    filters.stream = '';
    filters.start = '';
    filters.end = '';
    filters.pageSize = 25;
    pagination.page = 1;
    pagination.page_size = 25;
    load();
  }

  function setToday() {
    const start = new Date();
    start.setHours(0, 0, 0, 0);
    const end = new Date();
    filters.start = dateTimeLocalValue(start);
    filters.end = dateTimeLocalValue(end);
    apply();
  }

  function nextPage() {
    if (pagination.page * pagination.page_size >= pagination.total) return;
    pagination.page += 1;
    load();
  }

  function prevPage() {
    if (pagination.page <= 1) return;
    pagination.page -= 1;
    load();
  }

  function openDetail(row) {
    selected.value = row;
  }

  function closeDetail() {
    selected.value = null;
  }

  function formatDuration(ms) {
    const n = numberOrZero(ms);
    if (n >= 1000) return (n / 1000).toFixed(2) + 's';
    return Math.round(n) + 'ms';
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
    if (n === 0) return '$0.0000';
    if (Math.abs(n) < 0.0001) return '$' + n.toExponential(2);
    return '$' + n.toFixed(4);
  }

  function formatUnitPrice(v) {
    const n = numberOrZero(v);
    if (n <= 0) return '—';
    const perMillion = n * 1_000_000;
    if (perMillion >= 1) return '$' + round2(perMillion) + '/M';
    return '$' + perMillion.toFixed(4) + '/M';
  }

  function priceBrief(row) {
    if (!row) return '—';
    const input = formatUnitPrice(row.input_unit_price);
    const output = formatUnitPrice(row.output_unit_price);
    if (input === '—' && output === '—') return '—';
    return `${input.replace('/M', '')} / ${output}`;
  }

  function groupLabel(row) {
    const group = row?.group || 'default';
    const multiplier = numberOrZero(row?.group_multiplier) || 1;
    return `${group} • ${round2(multiplier)}x`;
  }

  function tokenLabel(row) {
    return row?.token_name || `#${row?.token_id || '—'}`;
  }

  function modelLabel(row) {
    return row?.model_name || row?.model || '—';
  }

  function modelInitial(row) {
    const name = modelLabel(row);
    if (!name || name === '—') return 'AI';
    return name.replace(/[^a-zA-Z0-9\u4e00-\u9fa5]/g, '').slice(0, 2).toUpperCase() || 'AI';
  }

  function streamRate(row) {
    const output = numberOrZero(row?.output_tokens);
    const totalMs = numberOrZero(row?.duration_ms);
    const frt = numberOrZero(row?.first_response_ms);
    const activeMs = Math.max(1, totalMs - (frt > 0 ? frt : 0));
    if (!row?.stream || output <= 0 || totalMs <= 0) return '—';
    return (output / (activeMs / 1000)).toFixed(1) + ' t/s';
  }

  function latencyLine(row) {
    return `${formatDuration(row?.duration_ms)} · FRT ${positive(row?.first_response_ms) ? formatDuration(row.first_response_ms) : '—'}`;
  }

  function cacheReadTokens(row) {
    return numberOrZero(row?.cache_read_tokens || row?.cache_tokens);
  }

  function cacheCreationTokens(row) {
    return numberOrZero(row?.cache_creation_tokens);
  }

  function reasoningTokens(row) {
    return numberOrZero(row?.reasoning_tokens);
  }

  function finalCost(row) {
    return numberOrZero(row?.actual_cost || row?.account_cost || row?.total_cost);
  }

  function billingMode(row) {
    return (row?.billing_mode || 'token') === 'token' ? '按 Token' : row?.billing_mode;
  }

  function errorDetail(row) {
    return String(row?.error || '').trim();
  }

  function pageRange() {
    if (!pagination.total) return '0 / 0';
    const start = (pagination.page - 1) * pagination.page_size + 1;
    const end = Math.min(pagination.page * pagination.page_size, pagination.total);
    return `${start}-${end} / ${pagination.total}`;
  }

  return {
    logs,
    loading,
    filters,
    pagination,
    summary,
    selected,
    load,
    apply,
    reset,
    setToday,
    nextPage,
    prevPage,
    openDetail,
    closeDetail,
    formatDuration,
    formatTokens,
    formatCost,
    formatUnitPrice,
    priceBrief,
    groupLabel,
    tokenLabel,
    modelLabel,
    modelInitial,
    streamRate,
    latencyLine,
    cacheReadTokens,
    cacheCreationTokens,
    reasoningTokens,
    finalCost,
    billingMode,
    errorDetail,
    pageRange,
  };
}
