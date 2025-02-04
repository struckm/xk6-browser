import launcher from 'k6/x/browser';
import { check } from 'k6';

export const options = {
  hosts: {'test.k6.io': '127.0.0.254'},
};

export default function() {
  const browser = launcher.launch('chromium', {
    headless: __ENV.XK6_HEADLESS ? true : false,
  });
  const context = browser.newContext();
  const page = context.newPage();

  const res = page.goto('http://test.k6.io/', { waitUntil: 'load' });

  check(res, {
    'null response': r => r === null,
  });

  page.close();
  browser.close();
}
