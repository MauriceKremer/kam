const navigation = document.querySelector('.manual-navigation');
const desktopLayout = window.matchMedia('(min-width: 768px)');

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