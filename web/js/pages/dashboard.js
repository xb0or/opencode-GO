/**
 * 仪表盘页面 - 组合式函数
 *
 * 负责加载统计数据、渲染图表、展示最近调用记录
 */

// Vue 全局 API (Vue 通过 CDN script 加载，全局可用)
const { ref, nextTick } = Vue;

export function useDashboard(api, showToast, t) {
  const stats = ref({});
  let chartModel = null;
  let chartProtocol = null;

  async function load() {
    try {
      const d = await api("/stats", "GET", null, t);
      stats.value = d;
      await nextTick();
      renderCharts(d);
    } catch (e) {
      showToast(e.message, "error");
    }
  }

  function renderCharts(d) {
    const mc = document.getElementById("chartModel");
    if (mc) {
      if (chartModel) chartModel.destroy();
      const labels = (d.by_model || []).map((m) => m.model);
      const values = (d.by_model || []).map((m) => m.count);
      chartModel = new Chart(mc, {
        type: "bar",
        data: {
          labels,
          datasets: [
            {
              label: "Calls",
              data: values,
              backgroundColor: "rgba(99,102,241,0.6)",
              borderColor: "rgba(99,102,241,1)",
              borderWidth: 1,
              borderRadius: 4,
            },
          ],
        },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          plugins: { legend: { display: false } },
          scales: {
            x: {
              ticks: { color: "#8892b0", font: { size: 10 } },
              grid: { display: false },
            },
            y: {
              ticks: { color: "#8892b0" },
              grid: { color: "rgba(42,45,58,0.5)" },
            },
          },
        },
      });
    }

    const pc = document.getElementById("chartProtocol");
    if (pc) {
      if (chartProtocol) chartProtocol.destroy();
      const labels = (d.by_protocol || []).map((p) => p.protocol);
      const values = (d.by_protocol || []).map((p) => p.count);
      const colors = ["#6366f1", "#10b981", "#f59e0b", "#ef4444"];
      chartProtocol = new Chart(pc, {
        type: "doughnut",
        data: {
          labels,
          datasets: [
            {
              data: values,
              backgroundColor: colors.slice(0, labels.length),
              borderWidth: 0,
            },
          ],
        },
        options: {
          responsive: true,
          maintainAspectRatio: false,
          plugins: {
            legend: {
              position: "bottom",
              labels: { color: "#8892b0", padding: 12 },
            },
          },
        },
      });
    }
  }

  return { stats, load };
}
