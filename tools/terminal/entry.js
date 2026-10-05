// What the console's terminal takes from xterm.js, and nothing else: the
// library, re-exported. How a terminal is put on the page — its look, the
// socket it speaks through, how it fits under the page's policy — is the
// console's own, in ui/console/js/terminal.js.
export {Terminal} from '@xterm/xterm';
export {FitAddon} from '@xterm/addon-fit';
export {WebglAddon} from '@xterm/addon-webgl';
