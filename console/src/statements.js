// PostgreSQL semicolons inside strings, comments, identifiers, and dollar quotes
// do not end a statement. Keep offsets so the editor can run the cursor statement.
export function splitStatements(sql) {
  const statements = [];
  let start = 0;
  let state = 'normal';
  let blockDepth = 0;
  let dollarTag = '';
  let escapedString = false;
  let hasCode = false;
  const push = (end) => {
    if (hasCode) statements.push({ sql: sql.slice(start, end).trim(), start, end });
    start = end;
    hasCode = false;
  };
  for (let i = 0; i < sql.length; i++) {
    const char = sql[i];
    const next = sql[i + 1];
    if (state === 'line') {
      if (char === '\n') state = 'normal';
      continue;
    }
    if (state === 'block') {
      if (char === '/' && next === '*') { blockDepth++; i++; }
      else if (char === '*' && next === '/') { blockDepth--; i++; if (!blockDepth) state = 'normal'; }
      continue;
    }
    if (state === 'single') {
      if (char === "'" && next === "'") i++;
      else if (char === '\\' && escapedString) i++;
      else if (char === "'") state = 'normal';
      continue;
    }
    if (state === 'double') {
      if (char === '"' && next === '"') i++;
      else if (char === '"') state = 'normal';
      continue;
    }
    if (state === 'dollar') {
      if (sql.startsWith(dollarTag, i)) { i += dollarTag.length - 1; state = 'normal'; }
      continue;
    }
    if (char === '-' && next === '-') { state = 'line'; i++; continue; }
    if (char === '/' && next === '*') { state = 'block'; blockDepth = 1; i++; continue; }
    if (char === "'") {
      escapedString = /[eE]/.test(sql[i - 1] ?? '') && !/[A-Za-z_0-9]/.test(sql[i - 2] ?? '');
      state = 'single'; hasCode = true; continue;
    }
    if (char === '"') { state = 'double'; hasCode = true; continue; }
    if (char === '$') {
      const tag = /^\$[A-Za-z_][A-Za-z_0-9]*\$|^\$\$/.exec(sql.slice(i))?.[0];
      if (tag) { state = 'dollar'; dollarTag = tag; hasCode = true; i += tag.length - 1; continue; }
    }
    if (char === ';') { push(i + 1); continue; }
    if (!/\s/.test(char)) hasCode = true;
  }
  push(sql.length);
  return statements;
}

export function statementAt(sql, offset) {
  const statements = splitStatements(sql);
  return statements.find((statement) => offset >= statement.start && offset < statement.end)
    ?? statements.findLast((statement) => statement.end <= offset)
    ?? statements[0];
}
