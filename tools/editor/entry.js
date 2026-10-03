// What the console's JSON box takes from CodeMirror 6, and nothing else: the
// library, re-exported. How the box is put together — its look, its verdict,
// the line a refusal lights — is the console's own, in ui/console/js/code.js.
export {EditorState, StateField, StateEffect, RangeSet} from '@codemirror/state';
export {EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, highlightSpecialChars, drawSelection, dropCursor,
  rectangularSelection, crosshairCursor, placeholder, Decoration, GutterMarker, gutterLineClass} from '@codemirror/view';
export {history, historyKeymap, defaultKeymap, indentWithTab} from '@codemirror/commands';
export {syntaxHighlighting, HighlightStyle, bracketMatching, indentOnInput, indentUnit, foldGutter, foldKeymap} from '@codemirror/language';
export {closeBrackets, closeBracketsKeymap} from '@codemirror/autocomplete';
export {search, searchKeymap, highlightSelectionMatches} from '@codemirror/search';
export {json} from '@codemirror/lang-json';
export {tags} from '@lezer/highlight';
