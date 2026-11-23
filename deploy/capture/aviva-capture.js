// Reads an Aviva workplace pension from the page it runs in, for `finstore import --format
// aviva-capture-json`. Run avivaCapture() on each of three pages: Overview, Contribution history with
// Full history selected, and Funds and investments. Each run returns that page's part as a JSON
// string. avivaCapture([part, part, part]) then returns the capture. It only reads the page, and
// every failure is a fixed error that carries nothing from the page.
function avivaCapture(parts) {
  const fail = (code) => {
    throw new Error("aviva-capture: " + code);
  };
  const money = (s) => {
    const m = /^£?\s*([0-9]{1,3}(?:,[0-9]{3})+|[0-9]+)\.([0-9]{2})$/.exec(String(s).trim());
    if (!m) fail("money");
    return m[1].replace(/,/g, "") + "." + m[2];
  };
  const decimal = (s) => {
    const v = String(s).trim().replace(/^£/, "").replace(/%$/, "").replace(/,/g, "");
    if (!/^[0-9]+(\.[0-9]+)?$/.test(v)) fail("decimal");
    return v;
  };
  const slashDate = (s) => {
    const m = /^([0-9]{2})\/([0-9]{2})\/([0-9]{4})$/.exec(String(s).trim());
    if (!m) fail("date");
    return m[3] + "-" + m[2] + "-" + m[1];
  };
  const months = ["January", "February", "March", "April", "May", "June", "July", "August",
    "September", "October", "November", "December"];
  const longDateRE = /^([0-9]{1,2}) ([A-Z][a-z]+) ([0-9]{4})$/;
  const longDate = (s) => {
    const m = longDateRE.exec(s);
    const month = m ? months.indexOf(m[2]) + 1 : 0;
    if (!month) fail("date");
    return m[3] + "-" + String(month).padStart(2, "0") + "-" + m[1].padStart(2, "0");
  };

  if (parts !== undefined) {
    const byPage = {};
    for (const raw of parts) {
      const part = JSON.parse(raw);
      if (byPage[part.page]) fail("repeated_page");
      byPage[part.page] = part;
    }
    const { overview, contributions, funds } = byPage;
    if (!overview || !contributions || !funds) fail("missing_page");
    if (overview.as_of !== funds.as_of) fail("as_of_differs");
    return JSON.stringify({
      schema_version: 1,
      captured_at: new Date().toISOString().replace(/\.[0-9]{3}Z$/, "Z"),
      as_of: overview.as_of,
      plan_ref: overview.plan_ref,
      total_value: overview.total_value,
      funds: funds.funds,
      contributions_complete: true,
      contributions_total: contributions.contributions_total,
      contributions: contributions.contributions,
      value_history: overview.value_history,
    });
  }

  const lines = document.body.innerText.split("\n").map((l) => l.trim()).filter(Boolean);
  const expect = (i, label) => {
    if (lines[i] !== label) fail("layout");
  };

  const fullHistory = document.getElementById("full-history");
  if (fullHistory) {
    if (!fullHistory.checked) fail("full_history_not_selected");
    const summary = lines.findIndex((l) => l.startsWith("Contribution summary ("));
    const history = lines.findIndex((l) => l.startsWith("Contribution history ("));
    if (summary < 0 || history < summary) fail("layout");
    const total = lines.indexOf("Total", summary);
    if (total < 0 || total > history) fail("layout");
    expect(total + 1, "Amount (Net)");
    expect(total + 3, "Amount (Gross)");
    const payers = { "Your employer": "employer", You: "member" };
    const contributions = [];
    for (let i = history + 1; i < lines.length && !lines[i].startsWith("Terms of use"); i += 9) {
      if (!longDateRE.test(lines[i])) fail("contribution_row");
      expect(i + 1, "Contribution type");
      expect(i + 3, "Who paid");
      expect(i + 5, "Amount (Net)");
      expect(i + 7, "Amount (Gross)");
      const payer = payers[lines[i + 4]];
      if (!payer) fail("payer");
      contributions.push({ date: longDate(lines[i]), type: lines[i + 2], payer, gross: money(lines[i + 8]) });
    }
    return JSON.stringify({ page: "contributions", contributions_total: money(lines[total + 4]), contributions });
  }

  const accurate = lines.map((l) => /^All values are accurate as of (.+)$/.exec(l)).find(Boolean);
  if (accurate) {
    const leaves = [...document.querySelectorAll("p")].filter((p) => p.children.length === 0 && p.offsetParent);
    const funds = leaves.filter((p) => p.textContent.trim() === "Units held").map((held) => {
      const card = held.closest(".m-card-content__inner");
      if (!card) fail("layout");
      const field = (label) => {
        const p = [...card.querySelectorAll("p")].find((e) => e.children.length === 0 && e.textContent.trim() === label);
        if (!p || !p.nextElementSibling) fail("layout");
        return p.nextElementSibling.textContent;
      };
      const heading = card.querySelector("h4");
      if (!heading) fail("layout");
      return {
        name: heading.textContent.trim().replace(/\s+/g, " "),
        units: decimal(field("Units held")),
        price: decimal(field("Unit price")),
        value: money(field("Fund value")),
        allocation_percent: decimal(field("Pension allocated %")),
      };
    });
    if (funds.length === 0) fail("no_funds");
    return JSON.stringify({ page: "funds", as_of: slashDate(accurate[1]), funds });
  }

  const plan = /Plan number:\s*([A-Z0-9]+)/.exec(document.body.innerText);
  const valueLine = lines.findIndex((l) => /^Your pension value as of [0-9/]+$/.test(l));
  if (!plan || valueLine < 0) fail("unknown_page");
  const table = [...document.querySelectorAll("table")].find((t) =>
    [...t.querySelectorAll("th")].map((h) => h.textContent.trim()).join("|") === "Date|Contributions to date|Total plan value");
  if (!table) fail("layout");
  const value_history = [...table.querySelectorAll("tbody tr")].map((row) => {
    const cells = [...row.children].map((c) => c.textContent.trim());
    if (cells.length !== 3) fail("layout");
    return { date: slashDate(cells[0]), contributions_to_date: money(cells[1]), value: money(cells[2]) };
  });
  return JSON.stringify({
    page: "overview",
    plan_ref: plan[1],
    as_of: slashDate(lines[valueLine].replace("Your pension value as of ", "")),
    total_value: money(lines[valueLine + 1]),
    value_history,
  });
}
