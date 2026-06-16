/**
 * ??????
 *
 * ???? /admin/usage???? sub2api UsageView ??????????
 */

const { reactive, ref, computed } = Vue;

function numberOrZero(v) {
  const n = Number(v);
  return Number.isFinite(n) ? n : 0;
}

export function useUsage(api, showToast, t) {
  const logs = ref([]);
  const loading = ref(false);
  const filters = reactive({ q: '', status: '', protocol: '', stream: '', pageSize: 25 });
  const pagination = reactive({ page: 1, page_size: 25, total: 0 });

  const summary = computed(() => {
    const rows = logs.value || [];
    const errors = rows.filter((r) => Number(r.status_code) >= 400).length;
    const avg = rows.length ? rows.reduce((sum, r) => sum + numberOrZero(r.duration_ms), 0) / rows.length : 0;
    return {
      visible: rows.length,
      errors,
      success: rows.length - errors,
      avg_duration_ms: avg,
      input_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.input_tokens), 0),
      output_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.output_tokens), 0),
      cache_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.cache_tokens), 0),
      cache_read_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.cache_read_tokens), 0),
      cache_creation_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.cache_creation_tokens), 0),
      total_tokens: rows.reduce((sum, r) => sum + numberOrZero(r.total_tokens), 0),
      total_cost: rows.reduce((sum, r) => sum + numberOrZero(r.total_cost), 0),
      actual_cost: rows.reduce((sum, r) => sum + numberOrZero(r.actual_cost), 0),
      account_cost: rows.reduce((sum, r) => sum + numberOrZero(r.account_cost), 0),
    };
  });

  function buildQuery() {
    const p = new URLSearchParams();
    p.set('page', String(pagination.page));
    p.set('page_size', String(filters.pageSize || pagination.page_size));
    if (filters.q.trim()) p.set('q', filters.q.trim());
    if (filters.status) p.set('status', filters.status);
    if (filters.protocol.trim()) p.set('protocol', filters.protocol.trim());
    if (filters.stream) p.set('stream', filters.stream);
    p.set('sort_by', 'created_at');
    p.set('sort_order', 'desc');
    return '?' + p.toString();
  }

  async function load() {
    loading.value = true;
    try {
      const d = await api('/usage' + buildQuery(), 'GET', null, t);
      logs.value = d.items || [];
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
    filters.pageSize = 25;
    pagination.page = 1;
    pagination.page_size = 25;
    load();
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
    return '$' + numberOrZero(v).toFixed(4);
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
    load,
    apply,
    reset,
    nextPage,
    prevPage,
    formatDuration,
    formatTokens,
    formatCost,
    pageRange,
  };
}
