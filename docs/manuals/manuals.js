const navigation = document.querySelector('.manual-navigation');
const desktopLayout = window.matchMedia('(min-width: 768px)');
const themeToggle = document.querySelector('.theme-toggle');

const isDark = () => document.documentElement.dataset.theme === 'dark';
const themeLabel = () => themeToggle.setAttribute('aria-label', `Switch to ${isDark() ? 'light' : 'dark'} mode`);

themeToggle.addEventListener('click', () => {
    const dark = !isDark();
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
    try { localStorage.setItem('kam-theme', dark ? 'dark' : 'light'); } catch {}
    themeLabel();
});

themeLabel();

function updateNavigation() {
    navigation.open = desktopLayout.matches;
}

updateNavigation();
desktopLayout.addEventListener('change', updateNavigation);

for (const table of document.querySelectorAll('article table')) {
    table.tabIndex = 0;
    table.setAttribute('aria-label', `${table.closest('section').querySelector('h2').textContent} reference table`);
}

for (const block of document.querySelectorAll('.prose pre code[class^="language-"]')) {
    const lang = block.className.replace(/^language-/, '');
    if (lang) block.parentElement.dataset.lang = lang.toUpperCase();
}

for (const diagram of document.querySelectorAll('.mermaid')) {
    diagram.tabIndex = 0;
    diagram.setAttribute('role', 'region');
    diagram.setAttribute('aria-label', `${diagram.closest('section').querySelector('h2').textContent} diagram`);
}