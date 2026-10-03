#!/usr/bin/env python3
"""docs/specs/original/*.docx 를 docs/specs/*.md 로 변환한다.

원본 docx가 개정되면 이 스크립트를 다시 실행해 Markdown을 재생성한다.
Markdown은 Claude Code와 리뷰어가 읽기 위한 파생본이며, 권위 원본은 docx다.

요구사항: pandoc (brew install pandoc), Python 3.9+
사용법:   python3 scripts/docs/convert_specs.py
"""
from __future__ import annotations

import re
import shutil
import subprocess
import tempfile
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SRC = ROOT / "docs" / "specs" / "original"
OUT = ROOT / "docs" / "specs"
ASSETS = OUT / "assets"

# 원본 파일 접두어 → (출력 파일명, 문서 제목)
DOCS = {
    "D01": ("D01-product-requirements.md", "D01 제품 요구사항과 벤치마크"),
    "D02": ("D02-system-data-api.md", "D02 시스템 데이터와 API 설계"),
    "D03": ("D03-instrumentation-diagnostics.md", "D03 계측과 고급 진단 설계"),
    "D04": ("D04-security-operations-commercial.md", "D04 보안 운영과 상용화"),
    "D05": ("D05-ux-ui-design.md", "D05 UX UI 디자인 명세"),
    "D06": ("D06-execution-quality-plan.md", "D06 개발 실행과 품질 계획"),
}

# docx에서 코드 문단은 Menlo 글꼴로만 구분된다. 이를 사용자 스타일로 표시해
# pandoc lua 필터가 연속 문단을 하나의 코드 블록으로 합치게 한다.
CODE_STYLE = "MontracerCode"
LUA_FILTER = r"""
local function is_code(b)
  return b.t == "Div" and b.attributes["custom-style"] == "MontracerCode"
end
local function guess_lang(text)
  if text:match("^%s*CREATE") or text:match("^%s*ALTER") then return "sql" end
  if text:match("^%s*[{%[]") then return "json" end
  local first = text:match("^[^\n]*")
  if first:match("^[%w_]+:%s*$") then return "yaml" end
  if text:match("^id:") or text:match("^event:") then return "text" end
  return "text"
end
local function flatten(blocks)
  local out = {}
  local buf = {}
  local function flush()
    if #buf > 0 then
      local text = table.concat(buf, "\n"):gsub("\u{A0}", " ")
      table.insert(out, pandoc.CodeBlock(text, {class = guess_lang(text)}))
      buf = {}
    end
  end
  for _, b in ipairs(blocks) do
    if is_code(b) then
      table.insert(buf, pandoc.utils.stringify(b))
    else
      flush()
      if b.t == "Div" and b.attributes["custom-style"] then
        for _, inner in ipairs(b.content) do table.insert(out, inner) end
      else
        table.insert(out, b)
      end
    end
  end
  flush()
  return out
end
function Pandoc(doc)
  doc.blocks = flatten(doc.blocks)
  return doc
end
"""


def mark_code_paragraphs(docx: Path, dest: Path) -> None:
    """Menlo 글꼴 run만 가진 문단에 CODE_STYLE pStyle을 추가한 사본을 만든다."""
    with zipfile.ZipFile(docx) as zin, zipfile.ZipFile(dest, "w", zipfile.ZIP_DEFLATED) as zout:
        for item in zin.infolist():
            data = zin.read(item.filename)
            if item.filename == "word/document.xml":
                xml = data.decode("utf-8")

                def tag(match: re.Match[str]) -> str:
                    para = match.group(0)
                    if 'w:ascii="Menlo"' not in para or "<w:pStyle" in para:
                        return para
                    # pandoc이 앞쪽 공백을 접으므로 들여쓰기를 NBSP로 보존한다 (lua에서 복원)
                    para = re.sub(r"(<w:t[^>]*>)( +)", lambda m: m.group(1) + "\u00a0" * len(m.group(2)), para)
                    if "<w:pPr>" in para:
                        return para.replace("<w:pPr>", f'<w:pPr><w:pStyle w:val="{CODE_STYLE}"/>', 1)
                    return re.sub(r"^<w:p(\s[^>]*)?>", lambda m: m.group(0) + f'<w:pPr><w:pStyle w:val="{CODE_STYLE}"/></w:pPr>', para, count=1)

                xml = re.sub(r"<w:p[ >].*?</w:p>", tag, xml, flags=re.S)
                data = xml.encode("utf-8")
            elif item.filename == "word/styles.xml":
                # pandoc은 styles.xml에 정의된 스타일만 custom-style로 인식한다
                style = (
                    f'<w:style w:type="paragraph" w:customStyle="1" w:styleId="{CODE_STYLE}">'
                    f'<w:name w:val="{CODE_STYLE}"/></w:style>'
                )
                data = data.decode("utf-8").replace("</w:styles>", style + "</w:styles>").encode("utf-8")
            zout.writestr(item, data)


def github_slug(text: str) -> str:
    text = text.strip().lower()
    text = re.sub(r"[^\w\- ]", "", text)
    return text.replace(" ", "-")


def postprocess(md: str, prefix: str, title: str, source_name: str) -> str:
    # 이미지: 절대 경로 → 상대 경로, HTML figure → Markdown 이미지
    def figure(m: re.Match[str]) -> str:
        src, alt = m.group(1), m.group(2)
        caption = re.sub(r"<[^>]+>", "", m.group(3)).strip()
        rel = "assets/" + src.split("/assets/", 1)[1]
        return f"![{alt}]({rel})\n\n*{caption}*"

    md = re.sub(
        r'<figure>\s*<img src="([^"]+)"[^>]*alt="([^"]*)"\s*/>\s*<figcaption>(.*?)</figcaption>\s*</figure>',
        figure,
        md,
        flags=re.S,
    )

    # 목차 링크를 실제 제목 slug에 맞춘다 (제목은 "## 01 제목" 형태)
    headings = re.findall(r"^## (\d{2} .+)$", md, flags=re.M)
    by_title = {h.split(" ", 1)[1]: h for h in headings}

    def toc(m: re.Match[str]) -> str:
        label = m.group(1)
        name = label.split(" ", 1)[1] if re.match(r"^\d{2} ", label) else label
        target = by_title.get(name)
        return f"[{label}](#{github_slug(target)})" if target else m.group(0)

    md = re.sub(r"\[([^\]]+)\]\(#[^)]+\)", toc, md)
    md = md.replace("Word 탐색 창에서도 제목별로 이동할 수 있다.", "").rstrip() + "\n"

    header = (
        f"# {title}\n\n"
        f"> Montracer 개발 문서 세트 v2.0 (기준일 2026-10-03) · 원본: "
        f"[`original/{source_name}`](original/{source_name.replace(' ', '%20')})  \n"
        f"> 이 파일은 `scripts/docs/convert_specs.py`로 생성한 파생본이다. 내용 변경은 원본 개정 + ADR로 한다.\n\n"
    )
    return header + md


def main() -> None:
    if shutil.which("pandoc") is None:
        raise SystemExit("pandoc이 필요합니다: brew install pandoc")
    with tempfile.TemporaryDirectory() as tmp:
        tmpdir = Path(tmp)
        lua = tmpdir / "code.lua"
        lua.write_text(LUA_FILTER, encoding="utf-8")
        for docx in sorted(SRC.glob("D0*.docx")):
            prefix = docx.name[:3]
            out_name, title = DOCS[prefix]
            marked = tmpdir / f"{prefix}.docx"
            mark_code_paragraphs(docx, marked)
            media_dir = ASSETS / prefix
            if media_dir.exists():
                shutil.rmtree(media_dir)
            result = subprocess.run(
                [
                    "pandoc", str(marked),
                    "-f", "docx+styles",
                    "-t", "gfm",
                    "--wrap=none",
                    "--shift-heading-level-by=1",
                    f"--lua-filter={lua}",
                    f"--extract-media={media_dir}",
                ],
                check=True, capture_output=True, text=True, cwd=tmpdir,
            )
            md = postprocess(result.stdout, prefix, title, docx.name)
            # pandoc은 media/ 하위에 추출한다 → assets/Dxx/ 로 평탄화
            nested = media_dir / "media"
            if nested.exists():
                for f in nested.iterdir():
                    f.rename(media_dir / f.name)
                nested.rmdir()
                md = md.replace(f"assets/{prefix}/media/", f"assets/{prefix}/")
            (OUT / out_name).write_text(md, encoding="utf-8")
            print(f"{docx.name} -> docs/specs/{out_name}")


if __name__ == "__main__":
    main()
