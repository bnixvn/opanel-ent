import React, { useEffect, useRef } from 'react';
import { EditorState } from '@codemirror/state';
import { EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter } from '@codemirror/view';
import { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
import { searchKeymap, highlightSelectionMatches } from '@codemirror/search';
import { bracketMatching, foldGutter, foldKeymap, indentOnInput, syntaxHighlighting, defaultHighlightStyle } from '@codemirror/language';
import { closeBrackets, closeBracketsKeymap, autocompletion, completionKeymap } from '@codemirror/autocomplete';
import { lintKeymap } from '@codemirror/lint';
import { html } from '@codemirror/lang-html';
import { javascript } from '@codemirror/lang-javascript';
import { css } from '@codemirror/lang-css';
import { json } from '@codemirror/lang-json';
import { php } from '@codemirror/lang-php';
import { sql } from '@codemirror/lang-sql';

// languageFor picks a mode from the filename.
//
// By extension rather than by sniffing the content: a half-written PHP file
// is still a PHP file, and guessing from content makes the highlighting flip
// about while somebody is typing.
function languageFor(name) {
  const ext = (name || '').toLowerCase().split('.').pop();
  switch (ext) {
    case 'php': case 'phtml': case 'inc':
      return [php()];
    case 'js': case 'mjs': case 'cjs': case 'jsx': case 'ts': case 'tsx':
      return [javascript({ jsx: ext.endsWith('x'), typescript: ext.startsWith('ts') })];
    case 'css': case 'scss': case 'less':
      return [css()];
    case 'json': case 'lock': case 'webmanifest':
      return [json()];
    case 'sql':
      return [sql()];
    case 'html': case 'htm': case 'twig': case 'vue':
      return [html()];
    default:
      // No mode at all for .txt, .ini, .conf, .log and everything else. A
      // wrong guess is worse than none: it colours half the file red and
      // makes a perfectly valid config look broken.
      return [];
  }
}

// CodeEditor is the file manager's text editor.
//
// Uncontrolled on purpose. Feeding every keystroke back through React state
// makes a large file crawl, so the view owns the text and only reports
// changes upward; the parent supplies the initial value and asks for the
// current one when it saves.
export default function CodeEditor({ value, filename, readOnly, onChange, onSave }) {
  const host = useRef(null);
  const view = useRef(null);
  const save = useRef(onSave);
  save.current = onSave;

  useEffect(() => {
    if (!host.current) return undefined;

    const state = EditorState.create({
      doc: value || '',
      extensions: [
        lineNumbers(),
        highlightActiveLineGutter(),
        highlightActiveLine(),
        highlightSelectionMatches(),
        history(),
        foldGutter(),
        indentOnInput(),
        bracketMatching(),
        closeBrackets(),
        autocompletion(),
        syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
        EditorView.lineWrapping,
        EditorState.readOnly.of(!!readOnly),
        keymap.of([
          // Ctrl-S is what everybody's fingers do in an editor, and without
          // it the browser opens a save-page dialog over the panel.
          {
            key: 'Mod-s',
            preventDefault: true,
            run: () => { if (save.current) save.current(); return true; },
          },
          indentWithTab,
          ...closeBracketsKeymap,
          ...defaultKeymap,
          ...searchKeymap,
          ...historyKeymap,
          ...foldKeymap,
          ...completionKeymap,
          ...lintKeymap,
        ]),
        ...languageFor(filename),
        EditorView.updateListener.of((u) => {
          if (u.docChanged && onChange) onChange(u.state.doc.toString());
        }),
      ],
    });

    view.current = new EditorView({ state, parent: host.current });
    return () => { view.current.destroy(); view.current = null; };
    // Rebuilt when the file changes, because the language and the document
    // are both baked into the state.
  }, [filename, readOnly]); // eslint-disable-line react-hooks/exhaustive-deps

  // A different file arriving in the same editor: replace the document
  // rather than the whole view, so scroll position and undo survive a save.
  useEffect(() => {
    const v = view.current;
    if (!v) return;
    const current = v.state.doc.toString();
    if (value !== undefined && value !== current) {
      v.dispatch({ changes: { from: 0, to: current.length, insert: value || '' } });
    }
  }, [value]);

  return <div className="editor" ref={host} />;
}
