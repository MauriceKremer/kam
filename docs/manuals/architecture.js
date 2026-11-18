// Progressive enhancement: the journeys are also written out as numbered steps on the page.
const journeys = [
    { id: 'all', label: 'Everything', summary: 'Pick a journey to highlight the parts and hand-offs it uses.' },
    { id: 'text', label: 'Group text received', summary: 'The radio hears a channel message. KAM decrypts it, saves it and offers it to your scripts.', steps: 'journey-text' },
    { id: 'room', label: 'Room packet received', summary: 'A MeshCore app talks to a room. The router hands the packet to the room server through the packet tap.', steps: 'journey-room' },
    { id: 'reply', label: 'Reply sent', summary: 'A script or a room answers. The message waits its turn in the transmit scheduler, then goes out through the writer loop.', steps: 'journey-reply' },
    { id: 'cfg', label: 'Settings changed', summary: 'You edit channels. KAM writes kam.yml and picks up the change without a restart.', steps: 'journey-cfg' },
    { id: 'api', label: 'REST request', summary: 'An operator tool asks KAM for status, events, channels, rooms or scripts over local HTTP.', steps: 'journey-api' },
];

const diagram = document.querySelector('#layer-diagram svg');
const controls = document.querySelector('#arch-controls');
const buttons = document.querySelector('#arch-flows');
const summary = document.querySelector('#arch-flow-summary');

if (diagram && controls && buttons && summary) {
    const parts = diagram.querySelectorAll('[data-flows]');

    function select(id) {
        const journey = journeys.find((item) => item.id === id);
        for (const part of parts) {
            const hot = part.dataset.flows.split(' ').includes(id);
            part.classList.toggle('is-hot', id !== 'all' && hot);
            part.classList.toggle('is-dim', id !== 'all' && !hot);
        }
        for (const button of buttons.children) {
            button.setAttribute('aria-pressed', String(button.dataset.journey === id));
        }
        summary.textContent = journey.summary + ' ';
        if (journey.steps) {
            const link = document.createElement('a');
            link.href = `#${journey.steps}`;
            link.textContent = 'Read the steps.';
            summary.append(link);
        }
    }

    for (const journey of journeys) {
        const button = document.createElement('button');
        button.type = 'button';
        button.dataset.journey = journey.id;
        button.textContent = journey.label;
        button.addEventListener('click', () => select(journey.id));
        buttons.append(button);
    }

    controls.hidden = false;
    select('all');
}
